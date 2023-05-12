package proxyd

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/ethereum/go-ethereum/common/math"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/log"
	"github.com/go-redis/redis/v8"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"golang.org/x/sync/semaphore"
)

func Start(config *Config) (*Server, func(), error) {
	if len(config.Backends) == 0 {
		return nil, nil, errors.New("must define at least one backend")
	}
	if len(config.BackendGroups) == 0 {
		return nil, nil, errors.New("must define at least one backend group")
	}
	if len(config.RPCMethodMappings) == 0 {
		return nil, nil, errors.New("must define at least one RPC method mapping")
	}

	for authKey := range config.Authentication {
		if authKey == "none" {
			return nil, nil, errors.New("cannot use none as an auth key")
		}
	}

	var redisClient *redis.Client
	if config.Redis.URL != "" {
		rURL, err := ReadFromEnvOrConfig(config.Redis.URL)
		if err != nil {
			return nil, nil, err
		}
		redisClient, err = NewRedisClient(rURL)
		if err != nil {
			return nil, nil, err
		}
	}

	if redisClient == nil && config.RateLimit.UseRedis {
		return nil, nil, errors.New("must specify a Redis URL if UseRedis is true in rate limit config")
	}

	var lim BackendRateLimiter
	var err error
	if config.RateLimit.EnableBackendRateLimiter {
		if redisClient != nil {
			lim = NewRedisRateLimiter(redisClient)
		} else {
			log.Warn("redis is not configured, using local rate limiter")
			lim = NewLocalBackendRateLimiter()
		}
	} else {
		lim = noopBackendRateLimiter
	}

	// While modifying shared globals is a bad practice, the alternative
	// is to clone these errors on every invocation. This is inefficient.
	// We'd also have to make sure that errors.Is and errors.As continue
	// to function properly on the cloned errors.
	if config.RateLimit.ErrorMessage != "" {
		ErrOverRateLimit.Message = config.RateLimit.ErrorMessage
	}
	if config.WhitelistErrorMessage != "" {
		ErrMethodNotWhitelisted.Message = config.WhitelistErrorMessage
	}
	if config.BatchConfig.ErrorMessage != "" {
		ErrTooManyBatchRequests.Message = config.BatchConfig.ErrorMessage
	}

	if config.SenderRateLimit.Enabled {
		if config.SenderRateLimit.Limit <= 0 {
			return nil, nil, errors.New("limit in sender_rate_limit must be > 0")
		}
		if time.Duration(config.SenderRateLimit.Interval) < time.Second {
			return nil, nil, errors.New("interval in sender_rate_limit must be >= 1s")
		}
	}

	maxConcurrentRPCs := config.Server.MaxConcurrentRPCs
	if maxConcurrentRPCs == 0 {
		maxConcurrentRPCs = math.MaxInt64
	}
	rpcRequestSemaphore := semaphore.NewWeighted(maxConcurrentRPCs)

	backendNames := make([]string, 0)
	backendsByName := make(map[string]*Backend)
	for name, cfg := range config.Backends {
		opts := make([]BackendOpt, 0)

		rpcURL, err := ReadFromEnvOrConfig(cfg.RPCURL)
		if err != nil {
			return nil, nil, err
		}
		wsURL, err := ReadFromEnvOrConfig(cfg.WSURL)
		if err != nil {
			return nil, nil, err
		}
		if rpcURL == "" {
			return nil, nil, fmt.Errorf("must define an RPC URL for backend %s", name)
		}

		if config.BackendOptions.ResponseTimeoutSeconds != 0 {
			timeout := secondsToDuration(config.BackendOptions.ResponseTimeoutSeconds)
			opts = append(opts, WithTimeout(timeout))
		}
		if config.BackendOptions.MaxRetries != 0 {
			opts = append(opts, WithMaxRetries(config.BackendOptions.MaxRetries))
		}
		if config.BackendOptions.MaxResponseSizeBytes != 0 {
			opts = append(opts, WithMaxResponseSize(config.BackendOptions.MaxResponseSizeBytes))
		}
		if config.BackendOptions.OutOfServiceSeconds != 0 {
			opts = append(opts, WithOutOfServiceDuration(secondsToDuration(config.BackendOptions.OutOfServiceSeconds)))
		}
		if config.BackendOptions.MaxDegradedLatencyThreshold > 0 {
			opts = append(opts, WithMaxDegradedLatencyThreshold(time.Duration(config.BackendOptions.MaxDegradedLatencyThreshold)))
		}
		if config.BackendOptions.MaxLatencyThreshold > 0 {
			opts = append(opts, WithMaxLatencyThreshold(time.Duration(config.BackendOptions.MaxLatencyThreshold)))
		}
		if config.BackendOptions.MaxErrorRateThreshold > 0 {
			opts = append(opts, WithMaxErrorRateThreshold(config.BackendOptions.MaxErrorRateThreshold))
		}
		if cfg.MaxRPS != 0 {
			opts = append(opts, WithMaxRPS(cfg.MaxRPS))
		}
		if cfg.MaxWSConns != 0 {
			opts = append(opts, WithMaxWSConns(cfg.MaxWSConns))
		}
		if cfg.Password != "" {
			passwordVal, err := ReadFromEnvOrConfig(cfg.Password)
			if err != nil {
				return nil, nil, err
			}
			opts = append(opts, WithBasicAuth(cfg.Username, passwordVal))
		}
		tlsConfig, err := configureBackendTLS(cfg)
		if err != nil {
			return nil, nil, err
		}
		if tlsConfig != nil {
			log.Info("using custom TLS config for backend", "name", name)
			opts = append(opts, WithTLSConfig(tlsConfig))
		}
		if cfg.StripTrailingXFF {
			opts = append(opts, WithStrippedTrailingXFF())
		}
		opts = append(opts, WithProxydIP(os.Getenv("PROXYD_IP")))
		opts = append(opts, WithSkipPeerCountCheck(cfg.SkipPeerCountCheck))

		back := NewBackend(name, rpcURL, wsURL, lim, rpcRequestSemaphore, opts...)
		backendNames = append(backendNames, name)
		backendsByName[name] = back
		log.Info("configured backend", "name", name, "rpc_url", rpcURL, "ws_url", wsURL)
	}

	backendGroups := make(map[string]*BackendGroup)
	for bgName, bg := range config.BackendGroups {
		backends := make([]*Backend, 0)
		for _, bName := range bg.Backends {
			if backendsByName[bName] == nil {
				return nil, nil, fmt.Errorf("backend %s is not defined", bName)
			}
			backends = append(backends, backendsByName[bName])
		}
		group := &BackendGroup{
			Name:     bgName,
			Backends: backends,
		}
		backendGroups[bgName] = group
	}

	var wsBackendGroup *BackendGroup
	if config.WSBackendGroup != "" {
		wsBackendGroup = backendGroups[config.WSBackendGroup]
		if wsBackendGroup == nil {
			return nil, nil, fmt.Errorf("ws backend group %s does not exist", config.WSBackendGroup)
		}
	}

	if wsBackendGroup == nil && config.Server.WSPort != 0 {
		return nil, nil, fmt.Errorf("a ws port was defined, but no ws group was defined")
	}

	for _, bg := range config.RPCMethodMappings {
		if backendGroups[bg] == nil {
			return nil, nil, fmt.Errorf("undefined backend group %s", bg)
		}
	}

	var resolvedAuth map[string]string

	if config.Authentication != nil {
		resolvedAuth = make(map[string]string)
		for secret, alias := range config.Authentication {
			resolvedSecret, err := ReadFromEnvOrConfig(secret)
			if err != nil {
				return nil, nil, err
			}
			resolvedAuth[resolvedSecret] = alias
		}
	}

	var (
		rpcCache    RPCCache
		blockNumLVC *EthLastValueCache
		gasPriceLVC *EthLastValueCache
	)
	if config.Cache.Enabled {
		var (
			cache      Cache
			blockNumFn GetLatestBlockNumFn
			gasPriceFn GetLatestGasPriceFn
		)

		if config.Cache.BlockSyncRPCURL == "" {
			return nil, nil, fmt.Errorf("block sync node required for caching")
		}
		blockSyncRPCURL, err := ReadFromEnvOrConfig(config.Cache.BlockSyncRPCURL)
		if err != nil {
			return nil, nil, err
		}

		if redisClient == nil {
			log.Warn("redis is not configured, using in-memory cache")
			cache = newMemoryCache()
		} else {
			cache = newRedisCache(redisClient)
		}
		// Ideally, the BlocKSyncRPCURL should be the sequencer or a HA replica that's not far behind
		ethClient, err := ethclient.Dial(blockSyncRPCURL)
		if err != nil {
			return nil, nil, err
		}
		defer ethClient.Close()

		blockNumLVC, blockNumFn = makeGetLatestBlockNumFn(ethClient, cache)
		gasPriceLVC, gasPriceFn = makeGetLatestGasPriceFn(ethClient, cache)
		rpcCache = newRPCCache(newCacheWithCompression(cache), blockNumFn, gasPriceFn, config.Cache.NumBlockConfirmations)
	}

	srv, err := NewServer(
		backendGroups,
		wsBackendGroup,
		NewStringSetFromStrings(config.WSMethodWhitelist),
		config.RPCMethodMappings,
		config.Server.MaxBodySizeBytes,
		resolvedAuth,
		secondsToDuration(config.Server.TimeoutSeconds),
		config.Server.MaxUpstreamBatchSize,
		rpcCache,
		config.RateLimit,
		config.SenderRateLimit,
		config.Server.EnableRequestLog,
		config.Server.MaxRequestBodyLogLen,
		config.BatchConfig.MaxSize,
		redisClient,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("error creating server: %w", err)
	}

	if config.Metrics.Enabled {
		addr := fmt.Sprintf("%s:%d", config.Metrics.Host, config.Metrics.Port)
		log.Info("starting metrics server", "addr", addr)
		go func() {
			if err := http.ListenAndServe(addr, promhttp.Handler()); err != nil {
				log.Error("error starting metrics server", "err", err)
			}
		}()
	}

	// To allow integration tests to cleanly come up, wait
	// 10ms to give the below goroutines enough time to
	// encounter an error creating their servers
	errTimer := time.NewTimer(10 * time.Millisecond)

	if config.Server.RPCPort != 0 {
		go func() {
			if err := srv.RPCListenAndServe(config.Server.RPCHost, config.Server.RPCPort); err != nil {
				if errors.Is(err, http.ErrServerClosed) {
					log.Info("RPC server shut down")
					return
				}
				log.Crit("error starting RPC server", "err", err)
			}
		}()
	}

	if config.Server.WSPort != 0 {
		go func() {
			if err := srv.WSListenAndServe(config.Server.WSHost, config.Server.WSPort); err != nil {
				if errors.Is(err, http.ErrServerClosed) {
					log.Info("WS server shut down")
					return
				}
				log.Crit("error starting WS server", "err", err)
			}
		}()
	} else {
		log.Info("WS server not enabled (ws_port is set to 0)")
	}

	for bgName, bg := range backendGroups {
		bgcfg := config.BackendGroups[bgName]
		if bgcfg.ConsensusAware {
			log.Info("creating poller for consensus aware backend_group", "name", bgName)

			copts := make([]ConsensusOpt, 0)

			if bgcfg.ConsensusAsyncHandler == "noop" {
				copts = append(copts, WithAsyncHandler(NewNoopAsyncHandler()))
			}
			if bgcfg.ConsensusBanPeriod > 0 {
				copts = append(copts, WithBanPeriod(time.Duration(bgcfg.ConsensusBanPeriod)))
			}
			if bgcfg.ConsensusMaxUpdateThreshold > 0 {
				copts = append(copts, WithMaxUpdateThreshold(time.Duration(bgcfg.ConsensusMaxUpdateThreshold)))
			}
			if bgcfg.ConsensusMaxBlockLag > 0 {
				copts = append(copts, WithMaxBlockLag(bgcfg.ConsensusMaxBlockLag))
			}
			if bgcfg.ConsensusMinPeerCount > 0 {
				copts = append(copts, WithMinPeerCount(uint64(bgcfg.ConsensusMinPeerCount)))
			}

			cp := NewConsensusPoller(bg, copts...)
			bg.Consensus = cp
		}
	}

	<-errTimer.C
	log.Info("started proxyd")

	shutdownFunc := func() {
		log.Info("shutting down proxyd")
		if blockNumLVC != nil {
			blockNumLVC.Stop()
		}
		if gasPriceLVC != nil {
			gasPriceLVC.Stop()
		}
		srv.Shutdown()
		if err := lim.FlushBackendWSConns(backendNames); err != nil {
			log.Error("error flushing backend ws conns", "err", err)
		}
		log.Info("goodbye")
	}

	return srv, shutdownFunc, nil
}

func secondsToDuration(seconds int) time.Duration {
	return time.Duration(seconds) * time.Second
}

func configureBackendTLS(cfg *BackendConfig) (*tls.Config, error) {
	if cfg.CAFile == "" {
		return nil, nil
	}

	tlsConfig, err := CreateTLSClient(cfg.CAFile)
	if err != nil {
		return nil, err
	}

	if cfg.ClientCertFile != "" && cfg.ClientKeyFile != "" {
		cert, err := ParseKeyPair(cfg.ClientCertFile, cfg.ClientKeyFile)
		if err != nil {
			return nil, err
		}
		tlsConfig.Certificates = []tls.Certificate{cert}
	}

	return tlsConfig, nil
}

func makeUint64LastValueFn(client *ethclient.Client, cache Cache, key string, updater lvcUpdateFn) (*EthLastValueCache, func(context.Context) (uint64, error)) {
	lvc := newLVC(client, cache, key, updater)
	lvc.Start()
	return lvc, func(ctx context.Context) (uint64, error) {
		value, err := lvc.Read(ctx)
		if err != nil {
			return 0, err
		}
		if value == "" {
			return 0, fmt.Errorf("%s is unavailable", key)
		}
		valueUint, err := strconv.ParseUint(value, 10, 64)
		if err != nil {
			return 0, err
		}
		return valueUint, nil
	}
}

func makeGetLatestBlockNumFn(client *ethclient.Client, cache Cache) (*EthLastValueCache, GetLatestBlockNumFn) {
	return makeUint64LastValueFn(client, cache, "lvc:block_number", func(ctx context.Context, c *ethclient.Client) (string, error) {
		blockNum, err := c.BlockNumber(ctx)
		return strconv.FormatUint(blockNum, 10), err
	})
}

func makeGetLatestGasPriceFn(client *ethclient.Client, cache Cache) (*EthLastValueCache, GetLatestGasPriceFn) {
	return makeUint64LastValueFn(client, cache, "lvc:gas_price", func(ctx context.Context, c *ethclient.Client) (string, error) {
		gasPrice, err := c.SuggestGasPrice(ctx)
		if err != nil {
			return "", err
		}
		return gasPrice.String(), nil
	})
}

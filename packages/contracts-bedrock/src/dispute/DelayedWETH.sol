// SPDX-License-Identifier: MIT
pragma solidity >=0.4.22 <0.6;

import { WETH9 } from "src/vendor/WETH9.sol";

// @title DelayedWETH
// @notice DelayedWETH is a contract that inherits from `WETH9` and allows users to wrap and unwrap
// WETH with a time delay. An ADMIN address can transfer WETH on behalf of any user and can unwrap
// DelayedWETH without a time delay.
contract DelayedWETH is WETH9 {
    event Unwrap(
        address indexed sender,
        uint256 wad,
        uint256 timestamp
    );

    struct Withdrawal {
        uint256 wad;
        uint256 timestamp;
    }

    mapping (address => Withdrawal) public withdrawals;

    uint256 internal immutable DELAY;
    address internal immutable ADMIN;

    constructor(uint256 delay, address admin) public {
        DELAY = delay;
        ADMIN = admin;
    }

    function transferFrom(address src, address dst, uint256 wad)
        public
        override
        returns (bool)
    {
        if (msg.sender == ADMIN) {
            allowance[src][msg.sender] = wad;
        }

        return super.transferFrom(rc, dst, wad);
    }

    function unwrap() public {
        Withdrawal storage wd = withdrawals[msg.sender];
        wd.wad = balanceOf(msg.sender);
        wd.timestamp = block.timestamp;
        emit Unwrap(msg.sender, wad, block.timestamp);
    }

    function withdraw(uint256 wad) public override {
        if (msg.sender != ADMIN) {
            Withdrawal storage wd = withdrawals[msg.sender];
            require(block.timestamp >= wd.timestamp + DELAY);
            require(wd.wad >= wad);
            require(balanceOf(msg.sender) >= wad);
            wd.wad -= wad;
        }

        msg.sender.transfer(wad);
        emit Withdrawal(msg.sender, wad);
    }
}

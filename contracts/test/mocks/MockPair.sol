// SPDX-License-Identifier: MIT
pragma solidity ^0.8.30;

/// @notice Minimal constant-product pair, enough for the indexer to read.
///
/// Shaped after a Uniswap v2 pair because that is what long-tail liquidity
/// mostly is: a single pool whose reserves are the whole story. The indexer
/// reads exactly these three calls on a real pair.
contract MockPair {
    address public token0;
    address public token1;
    uint112 private reserve0;
    uint112 private reserve1;
    uint32 private blockTimestampLast;

    event Swap(
        address indexed sender,
        uint256 amount0In,
        uint256 amount1In,
        uint256 amount0Out,
        uint256 amount1Out,
        address indexed to
    );

    constructor(address t0, address t1, uint112 r0, uint112 r1) {
        token0 = t0;
        token1 = t1;
        reserve0 = r0;
        reserve1 = r1;
        blockTimestampLast = uint32(block.timestamp);
    }

    function getReserves() external view returns (uint112, uint112, uint32) {
        return (reserve0, reserve1, blockTimestampLast);
    }

    /// @notice Move reserves, for exercising the indexer against a changing pool.
    function setReserves(uint112 r0, uint112 r1) external {
        reserve0 = r0;
        reserve1 = r1;
        blockTimestampLast = uint32(block.timestamp);
    }

    /// @notice Emit a swap, so volume can be indexed from logs.
    function recordSwap(uint256 amount0In, uint256 amount1In, uint256 amount0Out, uint256 amount1Out)
        external
    {
        emit Swap(msg.sender, amount0In, amount1In, amount0Out, amount1Out, msg.sender);
    }
}

/// @notice Minimal token exposing only what the indexer reads.
contract MockToken {
    string public name;
    uint8 public constant decimals = 18;
    uint256 public totalSupply;
    mapping(address => uint256) public balanceOf;

    constructor(string memory n, uint256 supply) {
        name = n;
        totalSupply = supply;
        balanceOf[msg.sender] = supply;
    }

    function setBalance(address who, uint256 amount) external {
        balanceOf[who] = amount;
    }
}

# MockUSDC test token

`MockUSDC.sol` is a test-only token with Circle USDC's EIP-3009 surface (6
decimals, EIP-712 domain name `USDC`, version `2`, `transferWithAuthorization`
with v/r/s, `authorizationState`). Anyone can `mint`. Never deploy it anywhere
real.

`MockUSDC.abi.json` and `MockUSDC.bin` are committed compiler output. The exact
command (solc 0.8.28 from the npm package `solc`, default EVM version, so the
bytecode uses PUSH0 and needs a Shanghai-or-later chain; the simulated backend
is one), run from the repo root, then rename the two outputs:

```bash
npx --yes solc@0.8.28 --abi --bin --optimize -o internal/x402/testdata internal/x402/testdata/MockUSDC.sol
mv internal/x402/testdata/internal_x402_testdata_MockUSDC_sol_MockUSDC.abi internal/x402/testdata/MockUSDC.abi.json
mv internal/x402/testdata/internal_x402_testdata_MockUSDC_sol_MockUSDC.bin internal/x402/testdata/MockUSDC.bin
```

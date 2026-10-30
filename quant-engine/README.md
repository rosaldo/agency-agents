# quant-engine

> A Go library that runs a trading strategy safely: it sizes every trade from a risk budget, places the protective stop on the exchange, enforces hard drawdown limits, and keeps a strategy in paper until its gates pass.

The engine is generic. It asks a strategy one question per closed candle — enter, exit or do nothing — and does everything else itself. It never knows which strategy it runs.

## The boundary: public engine, private strategies

| Public (this module) | Private (each strategy's own repository) |
|---|---|
| Strategy interface, CLI library (`start`, `status`, `stop`, `backtest`, `check`) | The strategy's rules: signal, stop and target |
| Sizing from the risk budget, risk limits, drawdown breaker | The `strategy:` block of the yml and its defaults |
| Order execution, exchange stops, reconciliation, journal | Its news content: feeds, keywords, aliases |
| Paper simulator, testnet demo, live gates, parameter hash | Its fixtures, skill text and examples |
| News guard **mechanism** (RSS/Atom reader, fail closed) | Its build and release of one binary per exchange |
| CCXT adapter (fork `github.com/rosaldo/ccxt/v5`) and generic indicators | |

A strategy repository imports this module, implements `strategy.Strategy`, and compiles one binary per exchange. This module ships no binary and no release of its own, and it contains no trading strategy: `strategy/example` is a trivial moving-average cross for tests and documentation, and the live gate refuses it by name.

## Quick start

A strategy binary is one call:

```go
package main

import (
	"github.com/rosaldo/agency-agents/quant-engine/cli"
	"github.com/rosaldo/agency-agents/quant-engine/exchange/ccxt/hyperliquid"
	"github.com/rosaldo/agency-agents/quant-engine/strategy/example"
)

func main() {
	cli.Main(cli.App{Strategy: example.New(), Venue: hyperliquid.Venue})
}
```

The binary name, the state directory (`.<name>/`), the default yml (`<name>.yml`) and the environment prefix all derive from `Strategy.Name()`. The venue package you import decides which exchange the binary links; the yml's `exchange` must name that venue.

```bash
my-strategy check                      # validate the yml: {"config":"ok"}
my-strategy backtest --candles btc.csv --capital 10000
my-strategy start                      # background loop, paper by default
my-strategy status                     # one JSON line: mode, P&L, limits, positions and their stops
my-strategy stop                       # positions stay protected by their exchange stops
my-strategy stop --flatten             # also close every position at market
```

## The strategy interface

```go
type Strategy interface {
	Name() string                     // stable id, ^[a-z0-9-]+$
	Configure(params yaml.Node) error // receives the yml `strategy:` block; validate and fail early
	Warmup() int                      // closed candles needed per Decide
	Inputs() []string                 // auxiliary symbols, e.g. a benchmark; nil = none
	Decide(in Input) Decision         // deterministic: backtest equals live
}
```

`Decide` receives only closed candles, the auxiliary candles and the engine's own position on the symbol (nil when flat). It returns `None`, `Exit`, or `Enter` with a side, a required stop and an optional target. The strategy never sees the exchange, the balance, orders, risk limits or credentials.

The engine refuses an `Enter` without a stop, with the stop on the wrong side of the price, or with a short when `perp.allow_short` is false, and journals why. Sizing, leverage, the liquidation check, order routing and every limit are the engine's.

Two optional interfaces extend a strategy:

- **`strategy.Trailer`** moves the stop by any rule the strategy defines (a moving average, ATR, structure). The engine applies an answer only when it tightens the stop, stays on the right side of the price and clear of the liquidation, and swaps the stop on the exchange without leaving the position unprotected. Backtest and live use the same rule.
- **`strategy.NewsSource`** supplies the news policy: feeds, keywords, aliases and lookback. See [News guard](#news-guard).

## Configuration

The engine validates every key outside `strategy:`, refuses unknown keys, and hands the `strategy:` block to `Configure`. This example loads as written with `strategy/example` (a test parses this block, so it stays valid):

```yaml
mode: paper                   # paper (default) | live
exchange: hyperliquid         # CCXT id; must be the venue linked into the binary
market: swap                  # spot | swap (linear perpetual)
asset_class: crypto           # crypto | equity (required)
symbols: [BTC/USDC:USDC]
interval: 1h
timezone: UTC
perp:
  leverage: 2
  margin_mode: isolated       # isolated (default) | cross
  allow_short: false
risk:
  max_risk_per_trade_pct: 1
  max_daily_drawdown_pct: 3
  max_weekly_drawdown_pct: 6
  min_demo_period_days: 30
  max_leverage: 3             # hard ceiling for perp.leverage
  min_liquidation_buffer: 0.5 # share of the distance to liquidation the stop must leave free
  confirmed_by_user: true     # set it yourself after reviewing the four limits above
demo: {source: testnet, candles: mainnet}   # optional; the only demo implemented
sim:                          # the only source of costs: paper simulator, backtest and sizing (see Known limitations)
  profile: real               # optimistic | real (default) | pessimistic
  liquidity_class: major      # major | liquid_alt | mid_alt | thin (required on a real exchange)
  # profiles:                 # optional: override a profile's built-in values
  #   pessimistic: {latency_ms: 2000, slippage_pct: 0.1, commission_pct: 0.05}
news_guard: none              # none | required; default none for crypto, required for equity
validation_file: validation.json
strategy:                     # opaque to the engine; this block is strategy/example's
  period: 20
  stop_pct: 2
```

- The four risk limits default to 1% per trade, 3% daily drawdown, 6% weekly drawdown and 30 demo days. The engine refuses to run until `risk.confirmed_by_user` is `true`.
- `asset_class: equity` forces `news_guard: required`; `news_guard: none` on an equity is refused.
- The parameter hash covers the engine's settings, `Name()`, the `strategy:` block and the strategy's news policy. Changing any of them starts the demo period over.
- Credentials never go in the yml: at any level, a key whose name contains `secret`, `token`, `passphrase`, `password`, `apikey`, `privatekey` or `mnemonic`, or has a `key` part (`api_key`, `private-key`), is refused.

### Credentials

Credentials come only from environment variables named `<STRATEGY>_<EXCHANGE>[_TESTNET]_<NAME>`, with the strategy and exchange in upper case and `-` replaced by `_`:

| Venue | Names |
|---|---|
| Hyperliquid | `WALLET_ADDRESS`, `PRIVATE_KEY` (an API wallet without withdrawal rights) |
| API-key exchanges | `API_KEY`, `API_SECRET`, optional `PASSPHRASE` |

Testnet and mainnet use separate variables (`_TESTNET_`), so a real credential never serves the demo. Credentials never appear in logs, the journal, `status` or error messages.

## Paper, demo and live gates

1. **Paper is the default.** With testnet credentials on a venue that has a sandbox (Hyperliquid), paper trades the testnet: real orders and stops, the exchange's own positions and liquidation, signal candles from mainnet. Without them, paper runs the engine's simulator (long and short, leverage, estimated liquidation).
2. **Live is refused** unless all of these hold:
   - the strategy is not the example strategy;
   - the paper journal, with the **same parameter hash**, shows activity on at least `min_demo_period_days` distinct calendar days;
   - `validation_file` reports `status: validated`, `method: walk-forward` and the same `params_hash`;
   - the user types **one** confirmation at the terminal after seeing the limits, `leverage`, `max_leverage`, `margin_mode`, `min_liquidation_buffer` and the capital. A confirmation that does not arrive in time is a refusal.
3. In paper, every call against the real account is refused by the adapter itself.

From then on the code holds the line: every entry is sized so that the loss at the stop stays within `max_risk_per_trade_pct`, the stop rests on the exchange, the stop lies between the entry and the liquidation with the buffer free, and the breaker halts trading at the daily or weekly drawdown while the exchange stops keep protecting what is open.

The engine manages only the orders and positions it opened. It records the client ID of each one and never touches quantity it did not open, so it needs no dedicated account. On a venue that nets one position per symbol, quantity the user added on the same side blocks new entries on that symbol, and the engine keeps managing only its own.

## Exchanges

Every exchange goes through CCXT, using the fork `github.com/rosaldo/ccxt/v5` (no builder fee, no referral, no broker ids). Only spot and linear swaps are traded.

- **Hyperliquid** is the supported venue: perpetuals on the main dex and on HIP-3 perp dexes (third-party markets such as equities, symbol `DEX-TICKER/USDC:USDC`), testnet demo, the exchange's liquidation price and its maintenance-margin formula.
- **CEX: basic, not verified per exchange.** Venue packages exist for `okx`, `binanceusdm` and `coinbaseinternational`, built only on CCXT's unified calls. They have no per-exchange verification and no tests against a real account. Check stops, cancels and position reports yourself on the exchange's testnet before relying on one.

Adding an exchange means adding a venue package; the engine itself has no per-exchange code.

## News guard

The engine provides the mechanism, not the content. With `news_guard: required`, before every entry the engine reads the strategy's RSS/Atom feeds and stays out when a recent headline names the asset together with one of the strategy's keywords.

The guard fails closed: no news policy, a policy without feeds, an unreadable feed, a timeout or a matching headline all keep the engine out. The engine ships no default feed, keyword or alias. A strategy that wants to trade with the guard on implements `strategy.NewsSource`; a policy with feeds but no keywords is refused at load.

## Known limitations

Read these before you trust a paper result.

- **Funding is not modeled in the paper simulator.** P&L and drawdown leave funding out, and `status` says so. The testnet demo and live journal the funding the venue reports.
- **Costs come from a profile, not from the live order book.** The `sim` profile is the only source of costs: the paper simulator, the backtest and the risk sizing of every trade use its slippage and taker fee (a yml with the old `costs` or `paper_sim` block is refused). In the paper simulator every fill (entry, exit, triggered stop) takes the price read `latency` after the order, moved by a fixed slippage against you, and pays the venue's base-tier taker fee. `sim.profile` picks the numbers; `sim.profiles` overrides any of them. **The backtest does not model latency:** it has only candles, and a delay of a few hundred milliseconds has no price inside a candle to land on, so it charges the slippage and the fee only. With `exchange: fake` there are no built-in numbers: costs are the liquidity class's slippage, if set, and the profile's overrides, else zero. A stop triggered by a closed candle fills at its candle price (no intra-candle price exists to delay to); one triggered by the last price waits the latency. Slippage is a fixed rate per liquidity class, measured in calm markets for a 10k USD order: larger orders and stressed markets cost more than the table says, and the pessimistic column only approximates that.

  | | optimistic | real | pessimistic |
  |---|---|---|---|
  | latency, hyperliquid | 250 ms | 550 ms | 1500 ms |
  | latency, okx | 150 ms | 400 ms | 1200 ms |
  | latency, binanceusdm | 150 ms | 350 ms | 1200 ms |
  | latency, coinbaseinternational | 150 ms | 300 ms | 1000 ms |
  | slippage, `major` (BTC, ETH) | 0.002% | 0.01% | 0.04% |
  | slippage, `liquid_alt` (e.g. SOL) | 0.005% | 0.02% | 0.08% |
  | slippage, `mid_alt` (e.g. AVAX) | 0.02% | 0.05% | 0.15% |
  | slippage, `thin` (small HIP-3 markets) | 0.05% | 0.25% | 0.6% |
  | taker fee, hyperliquid / okx / binanceusdm / coinbaseinternational | 0.045% / 0.05% / 0.05% / 0.035% | same | same |
  | taker fee, Hyperliquid HIP-3 market | 0.009% (growth mode) | 0.009% | 0.09% (no growth mode) |

  Public sources, read on 2026-10-30: fees from [Hyperliquid fees](https://hyperliquid.gitbook.io/hyperliquid-docs/trading/fees), [OKX fees](https://www.okx.com/fees), [Binance futures fees](https://www.binance.com/en/fee/futureFee) and [Deribit fees](https://support.deribit.com/hc/en-us/articles/25944746248989-Fees) (the venue behind the Coinbase International perp route); Hyperliquid latency from its [docs](https://hyperliquid.gitbook.io/hyperliquid-docs/hypercore/overview) (median 0.2 s, p99 0.9 s co-located) plus a measured round trip to each venue's public API; slippage from half spread plus impact on each venue's public order book. The HIP-3 fee is fixed at the growth-mode rate: the engine does not read a market's growth mode or deployer fee scale from the venue. Every venue other than these four needs its latency and fee set in `sim.profiles`.
- **The simulator treats `cross` as `isolated`.** It estimates liquidation with the isolated formula and caps the loss at the position's margin. The distance to liquidation is conservative, but the loss of a liquidation under cross margin, which can consume the whole account, is understated.
- **A stop and a liquidation in the same move resolve as a liquidation in the simulator.** The engine reads positions before candles, so a candle that crosses both the stop and the estimated liquidation liquidates, where the exchange would fill the resting stop first. This bias is pessimistic.
- **On an account that nets one position per symbol, the engine's stop covers only its own quantity (`own_qty`).** If the user trades the same symbol by hand, that quantity is not protected by the engine's stop.
- **The engine waits for confirmation before it reduces its own position.** When the account shows less than the engine's own quantity, the engine reduces `own_qty` (and its stop) only when a second reading at least one minute after the first still agrees. Until then it keeps the stop and takes no new entry on the symbol. A real reduction is therefore recognized with a delay of about a minute or more.
- **On a CEX account in hedge mode (separate positions per side), quantity the user holds on the same side as the engine is not detected.** Netting is off there, so only the engine's own fills count: the user's quantity does not block new entries, and the engine's stop does not cover it.

## Reserved: passive income modules

A second kind of module is reserved next to `Strategy` (trading): an allocation and yield module, with four named responsibilities — allocation, yield, limits and exit. Nothing of it exists yet: no package, interface, yml key or command. Package names are chosen so that a future `yield/` package fits beside `strategy/` without a rename.

## Versioning

This module lives in a subdirectory of its repository, so its versions are tags of the form `quant-engine/vX.Y.Z`. Until a version is tagged, consume it with a local `replace` directive in your `go.mod`.

## Development

```bash
go vet ./...
go test -race -count=1 ./...
```

Tests use the in-memory `fake` exchange (`exchange: fake`) and neutral fixtures in `testdata/`; none needs a network or an account.

# quant-engine

> A Go library that runs a trading strategy safely: it sizes every trade from a risk budget, places the protective stop on the exchange, enforces hard drawdown limits, and keeps a strategy in paper until its gates pass.

The engine is generic. It asks a strategy one question per closed candle — enter, exit or do nothing — and does everything else itself. It never knows which strategy it runs.

## The boundary: public engine, private strategies

| Public (this module) | Private (each strategy's own repository) |
|---|---|
| Strategy interface, CLI library (`start`, `status`, `stop`, `backtest`, `check`) | The strategy's rules: signal, stop and target |
| Sizing from the risk budget, risk limits, drawdown breaker | The `strategy:` block of the yml and its defaults |
| Order execution, exchange stops, reconciliation, journal | Its news content: feeds, keywords, aliases |
| Paper simulator, testnet demo, mainnet gates, parameter hash | Its fixtures, skill text and examples |
| News guard **mechanism** (RSS/Atom reader, fail closed) | Its build and release of one binary per exchange |
| CCXT adapter (fork `github.com/rosaldo/ccxt/v5`) and generic indicators | |

A strategy repository imports this module, implements `strategy.Strategy`, and compiles one binary per exchange. This module ships no binary and no release of its own, and it contains no trading strategy: `strategy/example` is a trivial moving-average cross for tests and documentation, and the mainnet gate refuses it by name.

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
my-strategy status                     # one JSON line: mode, P&L, limits, positions, their stops and targets
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
	Decide(in Input) Decision         // deterministic: backtest equals paper, testnet and mainnet
}
```

`Decide` receives only closed candles, the auxiliary candles and the engine's own position on the symbol (nil when flat). It returns `None`, `Exit`, or `Enter` with a side, a required stop and an optional target. The strategy never sees the exchange, the balance, orders, risk limits or credentials.

The engine refuses an `Enter` without a stop, with the stop on the wrong side of the price, or with a short when `perp.allow_short` is false, and journals why. A `None` returned while flat with a non-empty `Reason` is journaled as a skipped signal (`signal skipped: <reason>`), so a strategy that drops its own plan (no data for the stop, stop out of range) leaves a trail; a `None` without `Reason` means no signal and writes nothing. Sizing, leverage, the liquidation check, order routing and every limit are the engine's.

Graduated entry: `Decision.Fraction` (in (0, 1]; 0 means 1, today's single entry) is the part of the trade's risk budget an `Enter` or an `Add` takes. The budget is fixed at the first tranche (equity x `max_risk_per_trade_pct`) and kept with the fraction used in `state-<level>.json` (`trades`), saved before each tranche's order (a failed save sends no order), so a restart — even after a kill -9 between a fill and the end of the step — cannot renew it; a state without `trades` means no tranche in progress. `Add` is a further tranche of the engine's own open position, decided on the same closed candle as an `Enter`: sized against the position's stop (or a tighter one it brings; a looser one is refused), with the free margin, the leverage ceiling and the stop clear of the liquidation checked on the aggregate position. An `Add` while flat, on the other side, on a position without a recorded budget, or taking the fractions above 1 is refused and journaled. The strategy sees the trade in `Input.Position`: `BudgetUsed` is the fraction of the budget its tranches took so far (the sum of their `Fraction`s) and `Tranches` how many there were, both read from the saved `trades`, so live, paper and backtest see the same values and a restart sees what was saved. A tranche counts once saved, before its order: one whose order then fails stays counted, as its fraction does; a tranche whose save failed (`Enter` or `Add`: no order is sent, its margin stays free) and a refused `Add` count nothing. Both are 0 flat and on a position without a recorded budget; a state saved before tranches were counted shows at least 1 for a used budget. `status` shows each trade in progress under `trades`: `budget`, `budget_used` (the fraction used), `tranches` and `stop`; the state file keeps the fraction as `used`.

After each `Add` fills, the executor swaps the position's stop for one covering the whole quantity at the position's stop (or the tighter one the `Add` brought): the new stop is placed first and the old one cancelled after, so no instant leaves the added quantity unprotected. An old stop whose cancel fails is kept as stale and blocks the symbol, as for a trailing move (on spot, where both could sell, the position is closed at once); a new stop that cannot be placed leaves the start of the next step to cover the whole quantity, or to flatten the part the old stop misses and halt. The stop is then checked against the liquidation the exchange reports at the new average price, flattening and halting on a violation. A restart after a kill between an `Add`'s fill and the swap covers the whole quantity at the `Add`'s stop, with the trade budget as it was. The paper simulator goes through the same swap; a backtest trade entered in tranches reports the aggregate quantity and average price.

Four optional interfaces extend a strategy:

- **`strategy.Trailer`** moves the stop by any rule the strategy defines (a moving average, ATR, structure). The engine applies an answer only when it tightens the stop, stays on the right side of the price and clear of the liquidation, and swaps the stop on the exchange without leaving the position unprotected. Backtest, paper, testnet and mainnet use the same rule.
- **`strategy.Retargeter`** sets the exit target on the position as it actually filled: the engine sizes every tranche, so only it knows the real quantity and average price. Once an `Enter`'s or an `Add`'s fill is confirmed, the engine reads the position back from the exchange and calls `Retarget(in)` with the candle's `Input` and `in.Position` as filled (real `Qty` and `AvgPrice`, the stop, the target in force, `BudgetUsed`, `Tranches`). A positive answer replaces the target, 0 keeps it, a negative or non-finite one is refused; each change or refusal is journaled as `retarget`. The target is the engine's own check on each closed candle, never an order on the exchange, so backtest, paper, testnet and mainnet apply it alike; it is saved under `targets` in `state-<level>.json`, survives a restart, and `status` shows it under `targets`. The call is owed from before each tranche's order (`retarget_due`, written in the same save as the trade budget), put back as it was when nothing filled (the save failed, the order was never sent, or the exchange rejected it), and cleared once made, so a position unreadable after the fill (journaled), an error after the fill (a failed stop swap) or a kill before the end of the step leaves it owed: the symbol's next step, after a restart too, makes it on the position read then, before its target check. Meanwhile the `Decision`'s target stands. A retarget owed for a strategy that no longer implements the interface is dropped and journaled. A strategy without it behaves exactly as before: the `Decision`'s target, set before the fill.
- **`strategy.NewsSource`** supplies the news policy: feeds, keywords, aliases and lookback. See [News guard](#news-guard).
- **`strategy.DerivativesUser`** gets `Input.Deriv`: open interest, funding rate and mark price, one snapshot per candle. The snapshot of a closed candle is the one the engine reads in that candle's own step, after the close and before the decision. A read at or before the close, or in a later step, does not count, and a candle without a snapshot is marked missing. The engine refuses such a strategy at load on a spot market or on an adapter that does not read the data.

On swap markets every level (paper, testnet, mainnet) records the snapshots it reads to `deriv-<level>.csv` in the state directory: `time,symbol,candle,open_interest,funding_rate,mark_price`, where `time` is when the snapshot was read and `candle` is the open time of the candle it belongs to. The venue keeps no open interest history, so this file is the only source of it. A restart reloads the file and never writes a row twice. When the strategy does not need the data, the engine reads it after the step's trading actions, so a slow endpoint never delays them. If the file is unreadable (for example a crash cut the last line), a strategy that needs the data is refused; any other strategy keeps trading, and the journal says the recorder is off and which file to fix. The file itself is never changed. Read failures are journaled once per failure streak.

### Backtest with derivatives data

`backtest --deriv <file>` hands a `strategy.DerivativesUser` its snapshots, bound to the candles exactly as the loop bound them. The file is either the recorder's `deriv-<level>.csv` (rows of the yml's symbol) or Hyperliquid `fundingHistory` JSON: the responses as fetched, one array per call, appended (a call returns at most 500 rows from `startTime` on, and the next page starts at the last row's time, so it repeats that row). Every row must be of the yml symbol's coin (the base, or `dex:ticker` on a HIP-3 market), else the file is refused. From `fundingHistory` each candle gets the last rate settled at or before its close, with no open interest and no mark price; a candle closing after the history's last rate gets none.

Open interest is present only when the venue reported it: `market.Deriv.HasOpenInterest`. Funding history never has it, and the recorder writes an empty `open_interest` for a read without it; `OpenInterest` is then 0, not a reading. A rule on open interest must treat a snapshot without it as not passing, so it never runs as if its filter passed.

Live funding and the history may differ: the engine reads live funding through `fetchFundingRates`, which the Hyperliquid docs describe only as "current funding" in `metaAndAssetCtxs`, and the premium behind it "is sampled every 5 seconds and averaged over the hour" ([funding](https://hyperliquid.gitbook.io/hyperliquid-docs/trading/funding), [info endpoint, perpetuals](https://hyperliquid.gitbook.io/hyperliquid-docs/for-developers/api/info-endpoint/perpetuals)); `fundingHistory` holds the historical rates. Whether the current value read during the hour equals the rate later recorded for it is **not verified**: the docs do not say. Treat a funding-only replay as close to live, not identical.

The replay reads the file, never the venue. A strategy that needs the data is refused unless the snapshots span `--candles` from the first candle to the last, and on a spot market; it never runs as if its filter passed. One that also implements `strategy.OpenInterestUser` (`NeedsOpenInterest() bool`) is refused unless snapshots with open interest span them too, so a funding-only history refuses it instead of replaying an open interest filter that never passes. Any other strategy ignores `--deriv`. The summary's `candles_without_derivatives` counts the candles a strategy that needs the data got no snapshot for (recorded gaps, such as a failed read): a gap inside the span is not refused, the filter just does not pass there, so check this number before trusting a replay's trade count.

## Configuration

The engine validates every key outside `strategy:`, refuses unknown keys, and hands the `strategy:` block to `Configure`. This example loads as written with `strategy/example` (a test parses this block, so it stays valid):

```yaml
mode: paper                   # paper (default) | mainnet (real money; mode: live is refused)
exchange: hyperliquid         # CCXT id; must be the venue linked into the binary
market: swap                  # spot | swap (linear perpetual)
asset_class: crypto           # crypto | equity (required)
symbols: [BTC/USDC:USDC]
interval: 1h
feed: rest                    # rest (default) | websocket: how the loop learns a candle closed
timezone: UTC
perp:
  leverage: 2
  margin_mode: isolated       # isolated (default) | cross
  allow_short: false
risk:
  max_risk_per_trade_pct: 1
  max_daily_drawdown_pct: 3
  max_weekly_drawdown_pct: 6
  max_leverage: 3             # hard ceiling for perp.leverage
  min_liquidation_buffer: 0.5 # share of the distance to liquidation the stop must leave free
  confirmed_by_user: true     # set it yourself after reviewing the five limits above
demo: {source: testnet, candles: mainnet}   # optional; source: paper (simulator) | testnet (sandbox); candles: mainnet
gate:
  min_demo_days: 0            # optional; 0 (default) = no demo days required before mainnet
sim:                          # the only source of costs: paper simulator, backtest and sizing (see Known limitations)
  profile: real               # optimistic | real (default) | pessimistic
  liquidity_class: major      # major | liquid_alt | mid_alt | thin (required on a real exchange; with real fills even with a slippage_pct override)
  # profiles:                 # optional: override a profile's built-in values
  #   pessimistic: {latency_ms: 2000, slippage_pct: 0.1, commission_pct: 0.05}
news_guard: none              # none | required; default none for crypto, required for equity
validation_file: validation.json
strategy:                     # opaque to the engine; this block is strategy/example's
  period: 20
  stop_pct: 2
```

- The risk limits default to 1% per trade, 3% daily drawdown and 6% weekly drawdown. The engine refuses to run until `risk.confirmed_by_user` is `true`.
- `gate.min_demo_days` is optional and defaults to 0: mainnet needs no demo days. Set it above 0 to require that many days of demo first (see [Paper, demo and mainnet gates](#paper-demo-and-mainnet-gates)). The old `risk.min_demo_period_days` is refused.
- `asset_class: equity` forces `news_guard: required`; `news_guard: none` on an equity is refused.
- The parameter hash covers the engine's settings, `Name()`, the `strategy:` block and the strategy's news policy. Changing any of them invalidates the walk-forward validation and restarts any demo days `gate.min_demo_days` requires. `gate`, `demo` and `feed` are not part of it.
- `feed: rest` (default) steps 5 s after each candle boundary. `feed: websocket` steps on the venue's candle stream instead: the first update of the next candle says the candle ended, usually within a couple of seconds, and the step still runs 5 s after the boundary at the latest (a minute without a trade sends nothing; a dropped stream journals `feed_degraded`, then `feed_restored`, and the loop keeps the 5 s deadline meanwhile). The stream is only the trigger: the value decided on is always the closed candle read over REST, because the stream's last update of a candle is not always its final state. If REST's last closed candle is not the one the stream ended, that symbol takes no decision in that step (`candle_not_final`). Between steps the engine keeps each symbol's window and reads over REST only the candles closed since; it reads the whole window again at start and after the stream comes back. `websocket` is refused by `check` and at the loop's start on an adapter without a candle stream (only Hyperliquid has one) or on an interval its stream does not carry (Hyperliquid: 1m, 3m, 5m, 15m, 30m, 1h, 2h, 4h, 8h, 12h, 1d, 3d). The backtest is the same for both: it replays closed candles. `status` shows `feed` and, on `websocket`, `feed_state` (`ok` or `degraded`, and the stream's last update of each symbol's newest ended candle).
- A yml key that is not a string (an unquoted `true`, `false`, `null` or number) or a `.inf`/`.nan` value is refused at load by `check`, `backtest` and `start`: the parameter hash cannot take it. Quote such keys (`"true": ...`).
- Credentials never go in the yml: at any level, a key whose name contains `secret`, `token`, `passphrase`, `password`, `apikey`, `privatekey` or `mnemonic`, or has a `key` part (`api_key`, `private-key`), is refused.

### Credentials

Credentials come only from environment variables named `<STRATEGY>_<EXCHANGE>[_TESTNET]_<NAME>`, with the strategy and exchange in upper case and `-` replaced by `_`:

| Venue | Names |
|---|---|
| Hyperliquid | `WALLET_ADDRESS`, `PRIVATE_KEY` (an API wallet without withdrawal rights) |
| API-key exchanges | `API_KEY`, `API_SECRET`, optional `PASSPHRASE` |

Testnet and mainnet use separate variables (`_TESTNET_`), so a real credential never serves the demo. Credentials never appear in logs, the journal, `status` or error messages.

## Paper, demo and mainnet gates

1. **Paper is the default, at one of two levels picked by `demo.source`.** On a venue with a sandbox (Hyperliquid) there are three levels, `paper`, `testnet` and `mainnet`, which can run on the same day:
   - `paper`: the engine's simulator on mainnet's public candles (long and short, leverage, estimated liquidation, costs of the `sim` profile). It needs no credential and ignores testnet credentials, even where a sandbox exists;
   - `testnet`: the venue's sandbox with the testnet credentials (refused without them): real orders and stops, the exchange's own positions and liquidation, signal candles from mainnet;
   - `mainnet`: real money, run with `mode: mainnet`.

   Without `demo.source`, testnet credentials pick the testnet and their absence the simulator. A venue without a sandbox has only `paper` and `mainnet`.

   Each level keeps its own state and own-order book in the state directory (`state-paper.json`/`book-paper.json`, `state-testnet.json`/`book-testnet.json`, `state-mainnet.json`/`book-mainnet.json`) and records its level inside the state, so the three can share one `--dir` without one restoring another's breaker or positions; `status` reports `level`. A state file of another level, or one from before this split (`state-live.json`, `book-real.json`, `book-demo.json`, `state.json`, or a state without `level`), stops `start`, `run` and `status` with a message naming the file and what to rename it to; nothing is migrated automatically. `stop --flatten` checks only the files of the yml's level, so another level's file never blocks closing mainnet. Every such refusal also says that positions keep their exchange stops, if they have any, and how to close them now: fix the file and run `stop --flatten`, or close them on the exchange. `start` prints `started` with the `mode` and the `level` the loop runs at, and only after the loop passed its preconditions (feed, derivatives, state files, balance, markets, reconcile). If it refuses, `start` exits with the reason and `status` shows it in `stop_reason`, until the next start.
2. **Mainnet is refused** unless all of these hold:
   - the strategy is not the example strategy;
   - when `gate.min_demo_days` is above 0, the paper (demo) journal, with the **same parameter hash**, shows activity on at least that many distinct calendar days (none are required by default);
   - `validation_file` reports `status: validated`, `method: walk-forward` and the same `params_hash`;
   - the user types **one** confirmation at the terminal after seeing the limits, `leverage`, `max_leverage`, `margin_mode`, `min_liquidation_buffer` and the capital. A confirmation that does not arrive in time is a refusal.
3. In paper, every call against the mainnet account is refused by the adapter itself.

From then on the code holds the line: every entry is sized so that the loss at the stop stays within `max_risk_per_trade_pct`, the stop rests on the exchange, the stop lies between the entry and the liquidation with the buffer free, and the breaker halts trading at the daily or weekly drawdown while the exchange stops keep protecting what is open.

The engine manages only the orders and positions it opened. It records the client ID of each one and never touches quantity it did not open, so it needs no dedicated account. On a venue that nets one position per symbol, quantity the user added on the same side blocks new entries on that symbol, and the engine keeps managing only its own.

## Exchanges

Every exchange goes through CCXT, using the fork `github.com/rosaldo/ccxt/v5` (no builder fee, no referral, no broker ids). Only spot and linear swaps are traded.

- **Hyperliquid** is the supported venue: perpetuals on the main dex and on HIP-3 perp dexes (third-party markets such as equities, symbol `DEX-TICKER/USDC:USDC`), testnet demo, the exchange's liquidation price and its maintenance-margin formula.
- **CEX: basic, not verified per exchange.** Venue packages exist for `okx`, `binanceusdm` and `coinbaseinternational`, built only on CCXT's unified calls. They have no per-exchange verification and no tests against a real account. Check stops, cancels and position reports yourself on the exchange's testnet before relying on one.

Adding an exchange means adding a venue package; the engine itself has no per-exchange code.

## News guard

The engine provides the mechanism, not the content. With `news_guard: required` the engine reads the strategy's RSS/Atom feeds; a recent headline that names the asset together with one of the strategy's keywords is a hit. The asset's names are its base and that base's aliases; on a HIP-3 symbol (`XYZ-GOOGL/USDC:USDC`) they are the ticker and the dex, each with the aliases under its own key (`aliases: {GOOGL: [google]}` or `{XYZ: [...]}`). An `asset_class: equity` symbol that is not in that form is refused at load (`exchange: fake` excepted). A name from the symbol of up to 4 characters matches in its exact case, because tickers such as `NOW`, `NET`, `NOT` and `S` are also common words; a longer name and every alias match in any case. So "btc" in lowercase does not match `BTC` by itself: `aliases: {BTC: [btc, bitcoin]}` brings it back. A name from the symbol of one character (`S`, `W`) matches only through an alias (`aliases: {S: [sonic]}`), because of "U.S." and "S&P"; with `news_guard: required` and feeds, such a symbol without an alias is refused at load, since the guard would find nothing. On HIP-3 this applies to the ticker, with the alias under the ticker or the dex; a dex of one character needs none. A headline is compared one word at a time, letters and digits only, so an alias is one such word: with `news_guard: required` and feeds, an alias of the symbol that has a space or punctuation (`alphabet inc`, `dog-wif-hat`, `brk.b`) or is empty is refused at load, naming the symbol and the alias; list the words instead (`{GOOGL: [alphabet, google]}`). Alias keys are looked up in uppercase, tickers and dexes alike, so a key that is not uppercase (`{btc: [bitcoin]}`, `{xyz: [google]}`) is never read and is refused at load, naming the key. An uppercase key of a symbol the run does not trade is accepted. Keywords match the title and the description (its text, not its HTML), so a hit goes to the strategy's classifier with both, once per asset and headline (feed and text): a first line naming the asset (`Asset: BTC`, `Asset: GOOGL (dex xyz)`), then the title and the description as plain text (no HTML), at most 2000 characters in all; a text that does not fit goes as the title and what surrounds the matched words, so the classifier always reads what matched. The journal shows the title. Without a classifier every hit is a catastrophe (keywords alone).

When the check runs depends on `asset_class`:

- **`equity`: before the entry, blocking.** A catastrophe, an unreadable feed, a timeout, or a classifier that is unavailable keeps the engine out.
- **`crypto`: after the entry.** The entry is taken at once, and a worker checks the feeds at the entry and at every step while the position is open. The loop waits on the next candle and on the worker's verdicts together, so a catastrophe closes the position in the same cycle: the stop is cancelled and the position is closed at market, reduce-only (`emergency_exit`). The symbol then takes no new entry while the headline is within the lookback; the block is kept in the state file, so a restart does not lift it. An exit that fails is retried at the next step, and the stop on the exchange guards meanwhile. An unreadable feed leaves the position as it is, protected by its stop (`news_check_failed`), and is read again at the next step. The journal records each hit (`news_hit`) and how it was judged (`news_verdict`, with the classifier's `model`).

The classifier is TypeSafe's Jev (`POST https://api.typesafe.ai/v1/systemone`), asked one `noul` question per headline: a yes at or above the strategy's threshold is a catastrophe. A rate limited (`429`) or overloaded (`529`) answer is retried with exponential backoff (up to 3 retries from 250 ms) within the 10 s timeout. A hit whose classifier is unavailable, times out (10 s), fails, is still rate limited or overloaded after the retries, or has no key counts as a catastrophe, because a dangerous headline was found and nobody classified it. The API key is read only from the environment variable **`<STRATEGY>_TYPESAFE_API_KEY`** (the strategy's `Name()` upper-cased, `-` as `_`, e.g. `EXAMPLE_SMA_CROSS_TYPESAFE_API_KEY`; no venue or network in the name). It is sent only in the `Authorization` header, never written to the yml, a log, an error or the journal. `check` fails when a classifier is configured and the variable is unset. A classifier with `news_guard: none` is refused at load: nothing would ever ask it. Jev is trained mainly on English, so pick English feeds.

The guard fails closed. Without a news policy, or with a policy that lists no feeds, `required` takes no entry. Only RSS 2.0 (root `rss`) and Atom (root `feed`) are read: a feed whose answer is XML with another root, such as RSS 1.0 (`rdf:RDF`) or an XHTML page, is an unreadable feed, not an empty one, while RSS 2.0 or Atom with no items is a valid feed with no news. A feed that fails does not hide the others: every feed is read, and a hit in another one decides as if all had answered (a catastrophe keeps an equity out or closes a crypto position, and a hit judged noise lets the equity in), with the failure journaled as `news_check_failed`. With no hit in the others, the failure stands: an equity stays out, and a crypto position stays as it is. The engine ships no default feed, keyword, alias, question or threshold. A strategy that wants to trade with the guard on implements `strategy.NewsSource`. Its `NewsPolicy` gives the feeds, keywords, aliases and lookback (default 24 h), and an optional `Classifier`: `Model` (e.g. `jev-latest` or a pinned version), `Type` (`noul` only: a `choice` question is refused at load), `Question`, optional `Criteria` (`true`/`false`: what a yes and a no mean) and `Threshold` in (0, 1]. All of them are part of the parameter hash. A policy with feeds but no keywords, or a classifier missing a field, is refused at load.

## Known limitations

Read these before you trust a paper result.

- **The backtest never exits on news.** Past headlines cannot be replayed, so with `news_guard: required` a crypto backtest enters as it would live but never takes an emergency exit, and an equity backtest takes no entry. The backtest summary says so in `news`.
- **Open interest history exists only in our own recording.** The venue keeps none, so a backtest that needs open interest can only replay the period `deriv-<level>.csv` covers, and a walk-forward with an open interest filter is limited to the time recorded so far. `fundingHistory` gives funding only.
- **Funding is not modeled in the paper simulator.** P&L and drawdown leave funding out, and `status` says so. The testnet demo and mainnet journal the funding the venue reports.
- **Costs come from a profile, not from the live order book.** The `sim` profile is the only source of costs: the paper simulator, the backtest and the risk sizing of every trade use its slippage and taker fee (a yml with the old `costs` or `paper_sim` block is refused). **With real fills (mainnet, and the testnet demo) sizing never counts less than the built-in `real` profile:** it takes, field by field, the larger of the active profile (overrides included) and `real`, so the `optimistic` profile or a fee override below the venue's fee shrinks only simulated results (paper simulator and backtest), never the risk of a real trade. The same floored costs come off the distance to liquidation. With real fills `sim.liquidity_class` is required even when `slippage_pct` is overridden (it sets the floor's slippage), and on a venue with no built-in numbers a `commission_pct` or `slippage_pct` of 0 is refused (the override is the only floor). Only the engine's own simulator counts as simulated fills. `status` and the journal's `start` event show, in every mode, the profile and the slippage and fee sizing counts. In the paper simulator every fill (entry, exit, triggered stop) takes the price read `latency` after the order, moved by a fixed slippage against you, and pays the venue's base-tier taker fee. `sim.profile` picks the numbers; `sim.profiles` overrides any of them. **The backtest does not model latency:** it has only candles, and a delay of a few hundred milliseconds has no price inside a candle to land on, so it charges the slippage and the fee only. With `exchange: fake` there are no built-in numbers: costs are the liquidity class's slippage, if set, and the profile's overrides, else zero. A stop triggered by a closed candle fills at its candle price (no intra-candle price exists to delay to); one triggered by the last price waits the latency. Slippage is a fixed rate per liquidity class, measured in calm markets for a 10k USD order: larger orders and stressed markets cost more than the table says, and the pessimistic column only approximates that.

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

// Package cli is the command line of a strategy binary: a strategy's main is
// one call, cli.Main(cli.App{...}). It only dispatches subcommands; every rule
// lives in the engine's internal packages. The binary name, the state
// directory, the default yml and the environment prefix all derive from
// Strategy.Name().
package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata" // prebuilt binaries must resolve the yml timezone without system tzdata

	"github.com/rosaldo/agency-agents/quant-engine/backtest"
	"github.com/rosaldo/agency-agents/quant-engine/exchange"
	ccxtx "github.com/rosaldo/agency-agents/quant-engine/exchange/ccxt"
	"github.com/rosaldo/agency-agents/quant-engine/internal/config"
	"github.com/rosaldo/agency-agents/quant-engine/internal/engine"
	"github.com/rosaldo/agency-agents/quant-engine/internal/exchange/fake"
	"github.com/rosaldo/agency-agents/quant-engine/internal/exchange/paper"
	"github.com/rosaldo/agency-agents/quant-engine/internal/gate"
	"github.com/rosaldo/agency-agents/quant-engine/internal/journal"
	"github.com/rosaldo/agency-agents/quant-engine/internal/loop"
	"github.com/rosaldo/agency-agents/quant-engine/market"
	"github.com/rosaldo/agency-agents/quant-engine/strategy"
)

// App is one strategy binary: one strategy on one exchange venue, chosen at
// build time by the venue package the strategy's main imports.
type App struct {
	Strategy strategy.Strategy
	Venue    ccxtx.Venue // the exchange this binary links; the yml's exchange must name it (or fake)
	Version  string      // set by the strategy's build; "" = dev
}

const usage = `usage: NAME <command> [--config NAME.yml] [--dir .NAME]
  version
  check
  start    run the loop in the background (paper by default)
  status   one JSON line: alive, mode, P&L, limits, positions with their stops, why it stopped
  stop     end the loop; positions stay protected by their exchange stops
           --flatten also closes every position at market (stop cancelled first)
  backtest --candles <csv> --capital <n> [--aux <symbol>=<csv> ...] [--qty-step <n>] [--min-qty <n>] [--out <dir>]
           [--deriv <csv|json>]  derivatives data: the recorder's deriv-<level>.csv, or Hyperliquid fundingHistory JSON`

// Main runs the command line and exits 1 on error.
func Main(a App) {
	if err := a.Run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// envName is the strategy's environment prefix: "my-strategy" -> "MY_STRATEGY".
func (a App) envName() string {
	return strings.ToUpper(strings.ReplaceAll(a.Strategy.Name(), "-", "_"))
}

func (a App) readyEnv() string { return a.envName() + "_READY_FD" }

// Run dispatches one subcommand.
func (a App) Run(args []string, stdout io.Writer) error {
	if a.Strategy == nil {
		return errors.New("cli: no strategy")
	}
	name := a.Strategy.Name()
	use := strings.ReplaceAll(usage, "NAME", name)
	if len(args) == 0 {
		return errors.New(use)
	}
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	cfgPath := fs.String("config", name+".yml", "strategy config file")
	candles := fs.String("candles", "", "candle CSV of the config's symbol")
	aux := map[string]string{}
	fs.Func("aux", "candle CSV of a symbol the strategy also reads, as <symbol>=<csv> (repeatable)", func(v string) error {
		sym, file, ok := strings.Cut(v, "=")
		if !ok || sym == "" || file == "" {
			return errors.New("--aux takes <symbol>=<csv>")
		}
		aux[sym] = file
		return nil
	})
	deriv := fs.String("deriv", "", "derivatives data of the config's symbol: the recorder's deriv-<level>.csv, or Hyperliquid fundingHistory JSON (funding only)")
	capital := fs.Float64("capital", 0, "starting capital in quote currency")
	qtyStep := fs.Float64("qty-step", 0, "quantity increment (0 = fractional)")
	minQty := fs.Float64("min-qty", 0, "minimum order quantity")
	out := fs.String("out", "."+name+"/backtest", "output directory")
	dir := fs.String("dir", "."+name, "state directory (pid, state-<level>.json, book-<level>.json, journal.jsonl)")
	var fk fakeFlags
	fs.StringVar(&fk.candles, "fake-candles", "", "fake adapter: candle CSV of the config's symbol")
	fs.DurationVar(&fk.tick, "fake-tick", time.Second, "fake adapter: wall time per candle")
	fs.DurationVar(&fk.latency, "fake-latency", 0, "fake adapter: delay per exchange call")
	fs.Float64Var(&fk.cash, "fake-cash", 10_000, "fake adapter: starting cash")
	fs.Float64Var(&fk.paperCapital, "paper-capital", 10_000, "paper without exchange credentials: simulated capital")
	flatten := fs.Bool("flatten", false, "stop: also close every position at market")
	if err := fs.Parse(args[1:]); err != nil {
		return fmt.Errorf("%v\n%s", err, use)
	}
	switch args[0] {
	case "version":
		v := a.Version
		if v == "" {
			v = "dev"
		}
		_, err := fmt.Fprintln(stdout, v)
		return err
	case "status":
		return printStatus(*dir, stdout)
	case "stop":
		if *flatten && !loop.Locked(*dir) {
			if err := a.flattenNow(*cfgPath, *dir, fk); err != nil {
				return err
			}
			if err := printStatus(*dir, stdout); err != nil { // e.g. another level's old file
				return fmt.Errorf("stop --flatten closed the positions, but %w", err)
			}
			return nil
		}
		if err := loop.RequestStop(*dir, *flatten); err != nil {
			return err
		}
		return printStatus(*dir, stdout)
	}
	cfg, err := a.load(*cfgPath) // a bad strategy: block fails here, before anything runs
	if err != nil {
		return err
	}
	switch args[0] {
	case "check":
		if n := cfg.NewsPolicy(); n != nil && n.Classifier != nil && os.Getenv(config.NewsKeyVar(cfg.StrategyName())) == "" {
			return fmt.Errorf("news classifier: environment variable %s is not set", config.NewsKeyVar(cfg.StrategyName()))
		}
		var streamed []time.Duration // the fake adapter has no stream
		if cfg.Exchange != "fake" && a.Venue.DialFeed != nil {
			streamed = a.Venue.FeedIntervals
		}
		if err := loop.CheckFeedIntervals(cfg, streamed); err != nil {
			return err
		}
		_, err := fmt.Fprintln(stdout, `{"config":"ok"}`)
		return err
	case "backtest":
		return a.runBacktest(cfg, *cfgPath, *candles, *deriv, aux, *capital, *qtyStep, *minQty, *out, stdout)
	case "start":
		return a.start(cfg, *dir, args[1:], fk, stdout)
	case "run": // the background loop that start launches
		return a.runLoop(cfg, *dir, fk, stdout)
	default:
		return fmt.Errorf("unknown command %q\n%s", args[0], use)
	}
}

func (a App) runBacktest(cfg *config.Config, cfgPath, candles, deriv string, aux map[string]string, capital, qtyStep, minQty float64, out string, stdout io.Writer) error {
	if candles == "" || capital <= 0 {
		return errors.New("backtest needs --candles <csv> and --capital > 0")
	}
	yml, err := os.ReadFile(cfgPath)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	in := backtest.Input{Capital: capital, QtyStep: qtyStep, MinQty: minQty}
	if in.Candles, err = market.ReadCSVFile(candles); err != nil {
		return err
	}
	for sym, file := range aux {
		if in.Aux == nil {
			in.Aux = map[string][]market.Candle{}
		}
		if in.Aux[sym], err = market.ReadCSVFile(file); err != nil {
			return err
		}
	}
	if deriv != "" {
		if in.Deriv, err = readDeriv(deriv, cfg, in.Candles); err != nil {
			return err
		}
	}
	res, err := backtest.Run(context.Background(), a.Strategy, yml, in)
	if err != nil {
		return err
	}
	if err := res.Write(out); err != nil {
		return err
	}
	b, err := json.Marshal(res.Summary)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "%s\n", b)
	return err
}

// readDeriv reads the config symbol's snapshots from the recorder's file or,
// when it holds JSON, from Hyperliquid fundingHistory pages.
func readDeriv(path string, cfg *config.Config, cs []market.Candle) ([]market.Deriv, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("derivatives: %w", err)
	}
	if bytes.HasPrefix(bytes.TrimSpace(b), []byte("[")) {
		if len(cfg.Symbols) == 0 {
			return nil, nil
		}
		ds, err := market.ReadFundingHistory(bytes.NewReader(b), cfg.Symbols[0], cs, cfg.Interval())
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		return ds, nil
	}
	all, err := market.ReadDerivCSV(bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(cfg.Symbols) == 0 {
		return nil, nil
	}
	return all[cfg.Symbols[0]], nil
}

type fakeFlags struct {
	candles       string
	tick, latency time.Duration
	cash          float64
	paperCapital  float64
}

func env(cfg *config.Config) exchange.Env {
	if cfg.Mode == "mainnet" {
		return exchange.Real
	}
	return exchange.Demo
}

// newExchange builds the adapter named by the yml and the ticker that paces it.
func (a App) newExchange(cfg *config.Config, fk fakeFlags, dir string) (exchange.Exchange, loop.Ticker, error) {
	switch cfg.Exchange {
	case "fake":
		if fk.candles == "" || len(cfg.Symbols) != 1 {
			return nil, nil, errors.New("the fake adapter needs --fake-candles and exactly one symbol")
		}
		cs, err := market.ReadCSVFile(fk.candles)
		if err != nil {
			return nil, nil, err
		}
		f := fake.New(fake.Options{Paper: cfg.Mode == "paper", Cash: fk.cash, Latency: fk.latency,
			Candles: map[string][]market.Candle{cfg.Symbols[0]: cs}})
		return f, &loop.FakeTicker{Ex: f, Candles: cs, Delay: fk.tick}, nil
	}
	return a.newCCXT(cfg, fk, dir)
}

// load parses the yml for the strategy and checks it against the binary's
// venue: the yml's exchange must be the venue compiled in (or fake), and
// equities need a venue that lists them.
func (a App) load(path string) (*config.Config, error) {
	cfg, err := config.Load(path, a.Strategy)
	if err != nil {
		return nil, err
	}
	if cfg.Exchange == "fake" {
		return cfg, nil
	}
	switch {
	case a.Venue.ID == "" || a.Venue.Dial == nil:
		return nil, fmt.Errorf("exchange %q: this binary links no exchange venue", cfg.Exchange)
	case cfg.Exchange != a.Venue.ID:
		return nil, fmt.Errorf("exchange %q: this binary is built for %q; install the %s variant", cfg.Exchange, a.Venue.ID, cfg.Exchange)
	case cfg.AssetClass == "equity" && !a.Venue.Equity:
		return nil, fmt.Errorf("asset_class equity: %s lists no equity markets", a.Venue.ID)
	}
	return cfg, nil
}

// newCCXT builds the CCXT adapter of the binary's venue. Mainnet reads the
// mainnet credentials <STRATEGY>_<VENUE>_<SUFFIX>. Paper runs at the level
// demo.source picks: paper simulates fills on the venue's public mainnet data
// with no credential (perp included: long, short, leverage, estimated
// liquidation); testnet reads <STRATEGY>_<VENUE>_TESTNET_<SUFFIX> and trades
// the venue's sandbox with the signal's candles from mainnet. Without
// demo.source, testnet credentials pick the testnet, their absence the simulator.
func (a App) newCCXT(cfg *config.Config, fk fakeFlags, dir string) (exchange.Exchange, loop.Ticker, error) {
	v := a.Venue
	testnet := cfg.Mode == "paper"
	needCred := map[string]bool{"paper": false, "testnet": true, "": anySet(config.EnvPrefix(a.Strategy.Name(), v.ID, true), v.Credentials)}[cfg.Demo.Source]
	var cred map[string]string
	if !testnet || needCred {
		secrets, err := config.LoadCredentials(a.Strategy.Name(), v.ID, testnet, v.Credentials, v.Optional, os.Getenv)
		if err != nil {
			return nil, nil, err
		}
		cred = map[string]string{}
		for k, sv := range secrets {
			cred[k] = sv.Reveal() // the only place a secret becomes a string: handed to the venue's client
		}
		if err := cfg.CheckRealFills(); err != nil { // also the testnet its credentials picked
			return nil, nil, fmt.Errorf("config: %w", err)
		}
	}
	symbols := slices.Clone(cfg.Symbols)
	for _, aux := range a.Strategy.Inputs() {
		if !slices.Contains(symbols, aux) {
			symbols = append(symbols, aux)
		}
	}
	sandbox := testnet && cred != nil
	if sandbox {
		cfg.UseTestnet() // its own state and book: never the simulator's
	}
	client, err := v.Dial(cred, sandbox, symbols)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", v.ID, err)
	}
	var candles ccxtx.Client // the testnet demo trades the sandbox on mainnet's candles
	if sandbox {
		if candles, err = v.Dial(nil, false, symbols); err != nil {
			return nil, nil, fmt.Errorf("%s mainnet candles: %w", v.ID, err)
		}
	}
	var feed ccxtx.Watcher // feed: websocket; without one the loop refuses it at load
	if cfg.Feed == "websocket" && v.DialFeed != nil {
		if feed, err = v.DialFeed(symbols); err != nil {
			return nil, nil, fmt.Errorf("%s candle stream: %w", v.ID, err)
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, nil, err
	}
	ad, err := ccxtx.New(client, ccxtx.Options{Venue: v, Symbols: symbols, Market: cfg.Market, Env: env(cfg), Paper: testnet,
		Book: loop.BookPath(dir, cfg.Level()), Candles: candles, Feed: feed})
	if err != nil {
		return nil, nil, err
	}
	var tick loop.Ticker = loop.RealTicker{Interval: cfg.Interval(), Grace: grace}
	if feed != nil {
		tick = &loop.FeedTicker{Interval: cfg.Interval(), Grace: grace}
	}
	if cred == nil {
		return paper.New(ad, fk.paperCapital, time.Now, simProfile(cfg)), tick, nil
	}
	return ad, tick, nil
}

// grace is how long after a candle's close the loop steps at the latest:
// RealTicker always, FeedTicker when the stream says nothing.
const grace = 5 * time.Second

// simProfile is the yml's sim profile the simulator charges: latency, fee and slippage.
func simProfile(cfg *config.Config) paper.Profile {
	return paper.Profile{Name: cfg.Sim.Profile, Costs: cfg.SimCosts()}
}

// anySet reports whether any credential variable with prefix is set.
func anySet(prefix string, suffixes []string) bool {
	for _, s := range suffixes {
		if os.Getenv(prefix+s) != "" {
			return true
		}
	}
	return false
}

// start launches `run` in the background and waits until that process
// reports it holds the loop lock, passed the loop's preconditions and, in
// mainnet, passed the gates and got the typed confirmation itself. A refusal
// is start's error and status shows it. A second loop on the same directory
// is refused.
func (a App) start(cfg *config.Config, dir string, flags []string, fk fakeFlags, stdout io.Writer) (err error) {
	if err := loop.CheckRenamed(dir); err != nil {
		return err
	}
	if loop.Locked(dir) {
		return errors.New("already running; run stop first")
	}
	defer func() {
		if err != nil {
			_ = loop.SaveRefusal(dir, err.Error())
		}
	}()
	if cfg.Mode == "mainnet" {
		if err := gate.CheckLive(cfg, filepath.Join(dir, "journal.jsonl")); err != nil {
			return err
		}
	}
	if cfg.Exchange == "fake" { // a CCXT adapter is checked by the loop itself, once
		if _, _, err := a.newExchange(cfg, fk, dir); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := loop.ClearStop(dir); err != nil {
		return err
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	logf, err := os.OpenFile(filepath.Join(dir, "engine.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer logf.Close()
	readyR, readyW, err := os.Pipe()
	if err != nil {
		return err
	}
	defer readyR.Close()
	cmd := exec.Command(self, append([]string{"run"}, flags...)...)
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.ExtraFiles = []*os.File{readyW} // fd 3 in the child
	cmd.Env = append(os.Environ(), a.readyEnv()+"=3")
	detach(cmd, cfg.Mode == "mainnet")
	if err := cmd.Start(); err != nil {
		readyW.Close()
		return fmt.Errorf("start: %w", err)
	}
	readyW.Close()
	pid := cmd.Process.Pid
	_ = cmd.Process.Release()
	line := make(chan string, 1)
	go func() {
		l, _ := bufio.NewReader(readyR).ReadString('\n')
		line <- strings.TrimSpace(l)
	}()
	var level string // the child's: testnet credentials pick the sandbox only there
	select {
	case l := <-line:
		var ok bool
		if level, ok = strings.CutPrefix(l, "ready "); !ok {
			if l == "" {
				l = "the loop exited before starting; see " + filepath.Join(dir, "engine.log")
			}
			return errors.New(strings.TrimPrefix(l, "error: "))
		}
	case <-time.After(confirmTimeout + time.Minute):
		return errors.New("start: the loop did not report ready in time and may still come up: run status; see " + filepath.Join(dir, "engine.log"))
	}
	if err := os.WriteFile(filepath.Join(dir, "pid"), []byte(strconv.Itoa(pid)+"\n"), 0o600); err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "{\"started\":true,\"mode\":%q,\"level\":%q,\"pid\":%d}\n", cfg.Mode, level, pid)
	return err
}

// ready tells a waiting start how the launch went (a no-op when run was
// called directly). It carries no authority: mainnet is confirmed in this process.
func (a App) ready(msg string) {
	if os.Getenv(a.readyEnv()) != "3" {
		return
	}
	f := os.NewFile(3, "ready")
	_, _ = f.WriteString(strings.Join(strings.Fields(msg), " ") + "\n") // start reads one line
	f.Close()
}

// runLoop is the background process: it takes the loop lock, and in mainnet
// re-checks the gates and asks for the typed confirmation itself.
func (a App) runLoop(cfg *config.Config, dir string, fk fakeFlags, stdout io.Writer) (err error) {
	defer func() {
		if err != nil {
			a.ready("error: " + err.Error())
		}
	}()
	unlock, err := loop.Lock(dir)
	if err != nil {
		return err
	}
	defer unlock()
	if err := loop.CheckRenamed(dir); err != nil {
		return err
	}
	ex, tick, err := a.newExchange(cfg, fk, dir)
	if err != nil {
		return err
	}
	if cfg.Mode == "mainnet" {
		if err := gate.CheckLive(cfg, filepath.Join(dir, "journal.jsonl")); err != nil {
			return err
		}
		if err := confirmLive(cfg, ex); err != nil {
			return err
		}
		ignoreHangup()
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	started := false
	reason := loop.RunReady(ctx, cfg, a.Strategy, ex, env(cfg), tick, dir, func() {
		started = true
		_ = loop.ClearRefusal(dir)
		a.ready("ready " + cfg.Level())
	})
	if !started {
		return errors.New(reason)
	}
	_, err = fmt.Fprintln(stdout, reason)
	return err
}

func printStatus(dir string, stdout io.Writer) error {
	s, err := loop.ReadStatus(dir)
	if err != nil {
		return err
	}
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "%s\n", b)
	return err
}

// flattenNow closes positions directly when no loop is alive to do it.
func (a App) flattenNow(cfgPath, dir string, fk fakeFlags) error {
	unlock, err := loop.Lock(dir) // no loop may trade while this one closes positions
	if err != nil {
		return err
	}
	defer unlock()
	cfg, err := a.load(cfgPath)
	if err != nil {
		return err
	}
	ex, tick, err := a.newExchange(cfg, fk, dir) // picks the level: testnet credentials may pick the sandbox
	if err != nil {
		return err
	}
	if err := loop.CheckLevel(dir, cfg.Level()); err != nil { // only this level's files: another level's never blocks closing
		return err
	}
	j, err := journal.Open(dir, cfg.Mode, cfg.Level(), cfg.ParamsHash())
	if err != nil {
		return err
	}
	defer j.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return engine.New(cfg, a.Strategy, ex, env(cfg), j, tick.Now).Flatten(ctx)
}

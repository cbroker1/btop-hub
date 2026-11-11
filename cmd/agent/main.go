// Command btop-agent samples this machine once a second and streams it to the btop hub.
//
//	btop-agent --hub http://192.168.1.74:28677 --token TOKEN --name "GPU rig"
//	btop-agent --config agent.json
//	btop-agent --probe [--config agent.json]       print one sample and its sources, then exit
//	btop-agent --simulate z13 --hub ... --token ... feed plausible data for a named machine
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"btophub/internal/agent"
	"btophub/internal/collect"
	"btophub/internal/platform"
	"btophub/internal/proto"
)

func main() {
	var (
		configPath = flag.String("config", "", "JSON config file (hub, token, id, name)")
		hub        = flag.String("hub", "", "hub URL, e.g. http://192.168.1.74:28677")
		token      = flag.String("token", "", "shared agent token")
		tokenFile  = flag.String("token-file", "", "file holding the token")
		id         = flag.String("id", "", "machine id (default: slug of name)")
		name       = flag.String("name", "", "display name (default: hostname)")
		probe      = flag.Bool("probe", false, "print one sample as JSON with the source of every metric, then exit")
		simulate   = flag.String("simulate", "", "send simulated data for a machine: "+strings.Join(collect.SimProfiles, ", "))
		simUptime  = flag.Duration("sim-uptime", 0, "simulated time since boot (default per machine)")
		simGap     = flag.String("sim-gap", "", "simulated sleep gap as START,LENGTH ago, e.g. 40m,7m")
		version    = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()
	if *version {
		fmt.Println("btop-agent", proto.Version, runtime.GOOS+"/"+runtime.GOARCH)
		return
	}

	// Stay small: two OS threads for Go code, a modest heap.
	runtime.GOMAXPROCS(2)
	debug.SetGCPercent(50)
	debug.SetMemoryLimit(20 << 20)

	setupLog()

	var cfg agent.Config
	if *configPath != "" {
		c, err := agent.LoadConfig(*configPath)
		if err != nil {
			log.Fatalf("config: %v", err)
		}
		cfg = c
	}
	set := func(dst *string, v string) {
		if v != "" {
			*dst = v
		}
	}
	set(&cfg.Hub, *hub)
	set(&cfg.Token, *token)
	set(&cfg.TokenFile, *tokenFile)
	set(&cfg.ID, *id)
	set(&cfg.Name, *name)
	set(&cfg.Simulate, *simulate)
	if cfg.Simulate != "" {
		cfg.SimOpts = collect.DefaultSimOptions(cfg.Simulate)
		if *simUptime > 0 {
			cfg.SimOpts.Uptime = *simUptime
		}
		if *simGap != "" {
			a, b, _ := strings.Cut(*simGap, ",")
			s, err1 := time.ParseDuration(a)
			l, err2 := time.ParseDuration(b)
			if err1 != nil || err2 != nil {
				log.Fatalf("--sim-gap wants START,LENGTH like 40m,7m")
			}
			cfg.SimOpts.GapStart, cfg.SimOpts.GapLen = s, l
		}
		if cfg.Name == "" {
			cfg.Name = map[string]string{"gpu-rig": "GPU rig", "z13": "Z13", "gaming-rig": "Gaming rig"}[cfg.Simulate]
		}
	}
	if err := cfg.Resolve(); err != nil {
		log.Fatal(err)
	}

	if *probe {
		os.Exit(agent.Probe(cfg, os.Stdout))
	}

	run := func(ctx context.Context) error { return agent.Run(ctx, cfg) }
	if isSvc, err := platform.RunService("btop-agent", run); isSvc {
		if err != nil {
			log.Printf("service: %v", err)
			os.Exit(1)
		}
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx); err != nil {
		log.Fatal(err)
	}
}

// setupLog writes to a size-capped file when running as a Windows service, else stderr.
func setupLog() {
	log.SetFlags(log.LstdFlags)
	p := platform.LogPath()
	if p == "" {
		return
	}
	if fi, err := os.Stat(p); err == nil && fi.Size() > 1<<20 {
		os.Rename(p, p+".1")
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	log.SetOutput(io.MultiWriter(f))
}

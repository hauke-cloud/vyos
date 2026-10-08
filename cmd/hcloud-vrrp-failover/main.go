// Command hcloud-vrrp-failover moves Floating IPs, alias IPs and network
// routes to this server. VyOS runs it as the VRRP transition script when a
// group becomes master:
//
//	set high-availability vrrp group wan transition-script master '/usr/local/bin/hcloud-vrrp-failover wan'
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/hauke-cloud/vyos/internal/failover"
)

// version is set at build time.
var version = "dev"

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	flags := flag.NewFlagSet("hcloud-vrrp-failover", flag.ContinueOnError)
	configPath := flags.String("config", failover.DefaultConfigPath, "path to the configuration file")
	timeout := flags.Duration("timeout", 2*time.Minute, "give up after this long")
	serverID := flags.Int64("server-id", 0, "act as this server instead of asking the metadata service")
	showVersion := flags.Bool("version", false, "print the version and exit")
	flags.Usage = func() {
		_, _ = fmt.Fprint(flags.Output(), "Usage: hcloud-vrrp-failover [flags] <vrrp-group>\n\n")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *showVersion {
		fmt.Println(version)
		return 0
	}
	if flags.NArg() != 1 {
		flags.Usage()
		return 2
	}
	group := flags.Arg(0)

	// keepalived discards a script's output, so the journal is where this
	// has to be readable. stderr of a transition script ends up there.
	log := slog.New(slog.NewTextHandler(os.Stderr, nil)).With("group", group)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	if err := takeover(ctx, log, *configPath, group, *serverID); err != nil {
		log.Error("failover failed", "error", err)
		return 1
	}
	log.Info("failover complete")
	return 0
}

// retryInterval is how long to wait before trying a failed failover again.
const retryInterval = 2 * time.Second

func takeover(ctx context.Context, log *slog.Logger, configPath, groupName string, serverID int64) error {
	cfg, err := failover.LoadConfig(configPath)
	if err != nil {
		return err
	}
	group, err := cfg.Group(groupName)
	if err != nil {
		return err
	}
	token, err := cfg.ReadToken()
	if err != nil {
		return err
	}
	if serverID == 0 {
		if serverID, err = failover.ServerID(ctx, ""); err != nil {
			return err
		}
	}
	cloud := failover.NewHetznerCloud(token, failover.WithApplication("hcloud-vrrp-failover", version))
	return failover.TakeoverUntilDone(ctx, cloud, serverID, group, retryInterval, func(err error) {
		log.Warn("failover attempt failed, trying again", "error", err)
	})
}

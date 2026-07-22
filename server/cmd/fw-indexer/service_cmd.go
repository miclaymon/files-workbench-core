package main

import (
	"fmt"
	"log"
	"os"

	"files-workbench/v2/service"
)

// runServiceCommand handles the install/uninstall/service-status subcommands, which
// register fw-indexer with the OS service manager (see ../../../docs/DAEMON_PLAN.md).
func runServiceCommand(sub string, args []string) {
	mgr := service.NewManager()
	switch sub {
	case "install":
		// Reuse the run flags so `install` bakes the exact config the daemon will run
		// with (roots/addr/db), captured into the OS service definition.
		cfg := parseRunFlags(args)
		exe, err := os.Executable()
		if err != nil {
			log.Fatalf("[fw-indexer] cannot resolve own binary path: %v", err)
		}
		if err := mgr.Install(service.Config{Exe: exe, Env: serviceEnv(cfg)}); err != nil {
			log.Fatalf("[fw-indexer] install failed: %v", err)
		}
		fmt.Printf("fw-indexer installed and started\n  %s\n", mgr.Describe())

	case "uninstall":
		if err := mgr.Uninstall(); err != nil {
			log.Fatalf("[fw-indexer] uninstall failed: %v", err)
		}
		fmt.Println("fw-indexer uninstalled")

	case "service-status":
		st, err := mgr.Status()
		if err != nil {
			log.Fatalf("[fw-indexer] status check failed: %v", err)
		}
		fmt.Printf("%s\n  state: %s\n", mgr.Describe(), st)
	}
}

// serviceEnv builds the environment baked into the service definition: the resolved
// run config plus any FW_INDEX_* tuning vars present in the installer's environment.
// The daemon gets its whole configuration this way, since core (which normally
// passes these) may not be running when the OS starts the daemon.
func serviceEnv(cfg runConfig) map[string]string {
	env := map[string]string{
		"FW_INDEX_ROOTS": cfg.Roots,
		"FW_INDEX_ADDR":  cfg.Addr,
		"FW_INDEX_DB":    cfg.DB,
	}
	if d := os.Getenv("FW_DATA_DIR"); d != "" {
		env["FW_DATA_DIR"] = d
	}
	for _, k := range []string{"FW_INDEX_CONTENT", "FW_INDEX_CONTENT_BUDGET", "FW_INDEX_NATIVE"} {
		if v := os.Getenv(k); v != "" {
			env[k] = v
		}
	}
	return env
}

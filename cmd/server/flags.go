package main

import (
	"fmt"

	"github.com/urfave/cli/v2"

	"github.com/Layr-Labs/eigenda-proxy/eigenda"
	"github.com/Layr-Labs/eigenda-proxy/store"
	opservice "github.com/ethereum-optimism/optimism/op-service"
	oplog "github.com/ethereum-optimism/optimism/op-service/log"
	opmetrics "github.com/ethereum-optimism/optimism/op-service/metrics"
)

const (
	ListenAddrFlagName = "addr"
	PortFlagName       = "port"
)

const EnvVarPrefix = "EIGENDA_PROXY"

func prefixEnvVars(name string) []string {
	return opservice.PrefixEnvVar(EnvVarPrefix, name)
}

var (
	ListenAddrFlag = &cli.StringFlag{
		Name:    ListenAddrFlagName,
		Usage:   "server listening address",
		Value:   "127.0.0.1",
		EnvVars: prefixEnvVars("ADDR"),
	}
	PortFlag = &cli.IntFlag{
		Name:    PortFlagName,
		Usage:   "server listening port",
		Value:   3100,
		EnvVars: prefixEnvVars("PORT"),
	}
)

var requiredFlags = []cli.Flag{
	ListenAddrFlag,
	PortFlag,
}

var optionalFlags = []cli.Flag{}

func init() {
	optionalFlags = append(optionalFlags, oplog.CLIFlags(EnvVarPrefix)...)
	optionalFlags = append(optionalFlags, eigenda.CLIFlags(EnvVarPrefix)...)
	optionalFlags = append(optionalFlags, opmetrics.CLIFlags(EnvVarPrefix)...)
	optionalFlags = append(optionalFlags, store.CLIFlags(EnvVarPrefix)...)
	Flags = append(requiredFlags, optionalFlags...)
}

// Flags contains the list of configuration options available to the binary.
var Flags []cli.Flag

type CLIConfig struct {
	MemStoreCfg   store.MemStoreConfig
	EigenDAConfig eigenda.Config
	MetricsCfg    opmetrics.CLIConfig
}

func ReadCLIConfig(ctx *cli.Context) CLIConfig {
	return CLIConfig{
		EigenDAConfig: eigenda.ReadConfig(ctx),
		MetricsCfg:    opmetrics.ReadCLIConfig(ctx),
		MemStoreCfg:   store.ReadConfig(ctx),
	}
}

func (c CLIConfig) Check() error {
	err := c.EigenDAConfig.Check()
	if err != nil {
		return err
	}
	return nil
}

func CheckRequired(ctx *cli.Context) error {
	for _, f := range requiredFlags {
		if !ctx.IsSet(f.Names()[0]) {
			return fmt.Errorf("flag %s is required", f.Names()[0])
		}
	}
	return nil
}

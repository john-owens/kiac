package cmd

import (
	"fmt"

	"github.com/saiyam1814/kiac/pkg/cluster"
	"github.com/saiyam1814/kiac/pkg/ui"
	"github.com/spf13/cobra"
)

var (
	cacheCACertFiles []string
	cachePurge       bool
)

var cacheCmd = &cobra.Command{
	Use:   "cache",
	Short: "Manage the shared registry pull-through cache",
	Long: `The registry cache is one zot pull-through cache (` + cluster.RegistryCacheName + `)
shared by every cluster created with --registry-cache. It proxies docker.io,
registry.k8s.io, ghcr.io and quay.io, and keeps images on the named volume
` + cluster.RegistryCacheVolume + `, so they survive kiac delete cluster.`,
}

var cacheStartCmd = &cobra.Command{
	Use:   "start",
	Short: "Start the registry cache (create cluster --registry-cache does this automatically)",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		certs, err := cluster.LoadCABundle(cacheCACertFiles)
		if err != nil {
			return err
		}
		m := cluster.NewManager()
		var ip string
		if err := ui.Step("Starting registry cache "+cluster.RegistryCacheName, func() error {
			ip, err = m.EnsureRegistryCache(certs)
			return err
		}); err != nil {
			return err
		}
		// The cache may have come back on a new address; re-point every
		// running cluster that was created with --registry-cache.
		statuses, err := m.Statuses()
		if err != nil {
			return err
		}
		for _, s := range statuses {
			if err := m.HealRegistryCache(s.Name); err != nil {
				ui.Warnf("cluster %s: registry cache not re-pointed: %v", s.Name, err)
			}
		}
		ui.Successf("Registry cache is serving at http://%s:5000", ip)
		return nil
	},
}

var cacheStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show the registry cache state and address",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		state, ip, err := cluster.NewManager().RegistryCacheStatus()
		if err != nil {
			return err
		}
		if state == "" {
			fmt.Println("registry cache: not created (kiac cache start, or create cluster --registry-cache)")
			return nil
		}
		fmt.Printf("registry cache: %s %s\n", state, ip)
		return nil
	},
}

var cacheDeleteCmd = &cobra.Command{
	Use:   "delete",
	Short: "Remove the registry cache container (--purge also deletes cached images)",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := cluster.NewManager().DeleteRegistryCache(cachePurge); err != nil {
			return err
		}
		if cachePurge {
			ui.Successf("Registry cache and its cached images deleted")
		} else {
			ui.Successf("Registry cache container deleted; cached images kept on volume %s", cluster.RegistryCacheVolume)
		}
		return nil
	},
}

func init() {
	cacheStartCmd.Flags().StringArrayVar(&cacheCACertFiles, "ca-cert", nil, "PEM file of extra CA certificates the cache trusts for upstream pulls; repeatable")
	cacheDeleteCmd.Flags().BoolVar(&cachePurge, "purge", false, "also delete the volume holding every cached image")
	cacheCmd.AddCommand(cacheStartCmd, cacheStatusCmd, cacheDeleteCmd)
	rootCmd.AddCommand(cacheCmd)
}

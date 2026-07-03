package cmd

import (
	"context"
	"fmt"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/health"
	"github.com/spf13/cobra"
)

var (
	domainsService         string
	domainsWait            time.Duration
	domainsStrict          bool
	domainsExpectedTargets []string
	domainsHostsService    string
	domainsHostsAddress    string
)

var domainsCmd = &cobra.Command{
	Use:   "domains",
	Short: "Inspect public domains and internal route hosts",
}

var domainsStatusCmd = &cobra.Command{
	Use:          "status [domain...]",
	Short:        "Check DNS routing and TLS readiness for configured or ad-hoc domains",
	SilenceUsage: true,
	Args:         cobra.ArbitraryArgs,
	Long: `Check public domain DNS and TLS readiness without redeploying.

Without arguments, Tako checks the explicit proxy domains configured for the
selected environment. With domain arguments, Tako checks those domains as
ad-hoc domains against the same expected proxy targets.

The check is flexible: a domain is considered active when HTTPS works, even if
DNS resolves through a CDN or external proxy instead of directly to the VPS.`,
	RunE: runDomainsStatus,
}

var domainsHostsCmd = &cobra.Command{
	Use:          "hosts",
	Short:        "Print /etc/hosts entries for internal proxy routes",
	SilenceUsage: true,
	Long: `Print host-file entries for services configured with proxy.visibility=internal.

By default, Tako maps each internal host to servers.NODE.privateHost when it
is configured and falls back to the generated mesh IP for that proxy node.`,
	RunE: runDomainsHosts,
}

func init() {
	rootCmd.AddCommand(domainsCmd)
	domainsCmd.AddCommand(domainsStatusCmd)
	domainsCmd.AddCommand(domainsHostsCmd)
	domainsStatusCmd.Flags().StringVarP(&domainsService, "service", "s", "", "Only check configured domains for one service")
	domainsStatusCmd.Flags().DurationVar(&domainsWait, "wait", 0, "Wait up to this duration for DNS/TLS to become active (for example 2m); 0 checks once")
	domainsStatusCmd.Flags().BoolVar(&domainsStrict, "strict", false, "Exit non-zero if any checked domain is not active")
	domainsStatusCmd.Flags().StringArrayVar(&domainsExpectedTargets, "target", nil, "Expected DNS target; repeat for custom edge/CNAME targets (defaults to proxy server hosts)")
	domainsHostsCmd.Flags().StringVarP(&domainsHostsService, "service", "s", "", "Only print internal hosts for one service")
	domainsHostsCmd.Flags().StringVar(&domainsHostsAddress, "address", "auto", "Address source: auto, private, mesh, or ssh")
}

func runDomainsStatus(cmd *cobra.Command, args []string) error {
	cfg, err := config.LoadConfig(cfgFile)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}
	envName := getEnvironmentName(cfg)
	env, err := cfg.GetEnvironment(envName)
	if err != nil {
		return err
	}

	specs := collectConfiguredDomainSpecs(env.Services, domainsService)
	if len(args) > 0 {
		specs = make([]domainStatusSpec, 0, len(args))
		serviceName := domainsService
		if serviceName == "" {
			serviceName = "ad-hoc"
		}
		for _, domain := range args {
			specs = append(specs, domainStatusSpec{Service: serviceName, Domain: domain, Role: "ad-hoc"})
		}
	}
	if len(specs) == 0 {
		if domainsService != "" {
			return fmt.Errorf("service %s has no configured public domains", domainsService)
		}
		fmt.Println("No configured public domains found.")
		return nil
	}

	targets, err := domainExpectedTargets(cfg, envName, domainsExpectedTargets)
	if err != nil {
		return err
	}
	_, err = monitorDomainStatuses(context.Background(), health.NewHealthChecker(), specs, domainStatusOptions{
		Timeout:         domainsWait,
		Strict:          domainsStrict,
		ExpectedTargets: targets,
	})
	return err
}

package cmd

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/engine"
	"github.com/redentordev/tako-cli/pkg/health"
	"github.com/redentordev/tako-cli/pkg/takoapi"
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

The probe reports a domain active when HTTPS works, even if DNS resolves through
a CDN or external proxy instead of directly to the VPS. Strict mode accepts
that heuristic only when the configured service declares proxy.cdn.`,
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
	domainsStatusCmd.Flags().BoolVar(&domainsStrict, "strict", false, "Exit non-zero unless every domain is active and any detected CDN is explicitly declared")
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

	configuredSpecs := collectConfiguredDomainSpecs(env.Services, domainsService)
	specs := configuredSpecs
	if len(args) > 0 {
		specs = collectAdHocDomainSpecs(args, configuredSpecs, domainsService)
	}

	result := engine.DomainsResult{
		APIVersion:  takoapi.APIVersionCurrent,
		Kind:        engine.KindDomainsResult,
		Project:     cfg.Project.Name,
		Environment: envName,
		Service:     domainsService,
		Domains:     []engine.DomainStatusEntry{},
	}

	if len(specs) == 0 {
		if domainsService != "" {
			return &engine.InvalidRequestError{Err: fmt.Errorf("service %s has no configured public domains", domainsService)}
		}
		var out io.Writer = os.Stdout
		if machineOutputEnabled() {
			out = os.Stderr
		}
		fmt.Fprintln(out, "No configured public domains found.")
		result.AllActive = true
		return emitResultDocument(result)
	}

	targets, err := domainExpectedTargets(cfg, envName, domainsExpectedTargets)
	if err != nil {
		return err
	}
	result.ExpectedTargets = targets
	statuses, err := monitorDomainStatuses(cmd.Context(), health.NewHealthChecker(), specs, domainStatusOptions{
		Timeout:         domainsWait,
		Strict:          domainsStrict,
		ExpectedTargets: targets,
	})

	// MonitorDomainStatuses returns statuses in spec order.
	result.AllActive = true
	for index, status := range statuses {
		entry := engine.DomainStatusEntry{
			Service:     status.Service,
			Domain:      status.Domain,
			State:       string(status.State),
			DNS:         string(status.DNS),
			TLS:         string(status.TLS),
			ResolvedIPs: status.ResolvedIPs,
			CNAME:       status.CNAME,
			CDN:         status.CDN,
			Message:     status.Message,
			Warning:     status.Warning,
			DNSError:    status.DNSError,
			TLSError:    status.TLSError,
		}
		if index < len(specs) {
			entry.Role = specs[index].Role
		}
		if status.Pending() {
			result.AllActive = false
		}
		result.Domains = append(result.Domains, entry)
	}
	if emitErr := emitResultDocument(result); emitErr != nil && err == nil {
		err = emitErr
	}
	return err
}

func collectAdHocDomainSpecs(domains []string, configured []domainStatusSpec, serviceFilter string) []domainStatusSpec {
	configuredByDomain := make(map[string]domainStatusSpec, len(configured))
	for _, spec := range configured {
		configuredByDomain[domainStatusKey(spec.Domain)] = spec
	}
	specs := make([]domainStatusSpec, 0, len(domains))
	for _, domain := range domains {
		serviceName := serviceFilter
		cdn := ""
		if match, ok := configuredByDomain[domainStatusKey(domain)]; ok {
			cdn = match.CDN
			if serviceName == "" {
				serviceName = match.Service
			}
		}
		if serviceName == "" {
			serviceName = "ad-hoc"
		}
		specs = append(specs, domainStatusSpec{Service: serviceName, Domain: domain, Role: "ad-hoc", CDN: cdn})
	}
	return specs
}

func domainStatusKey(domain string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(domain)), ".")
}

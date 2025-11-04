package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"sync"
	"syscall"

	"gopkg.in/yaml.v3"

	"k8s.io/klog/v2"

	"github.com/ydb-platform/udev-manager/internal/mux"
	"github.com/ydb-platform/udev-manager/internal/plugin"
	"github.com/ydb-platform/udev-manager/internal/udev"
)

func main() {
	appContext, appCancel := context.WithCancel(context.Background())
	appWaitGroup := &sync.WaitGroup{}
	defer appWaitGroup.Wait()
	defer appCancel()

	flags := initFlags()

	// Registry creates a separate plugin for each resource registered
	registry, err := plugin.NewRegistry(appContext, appWaitGroup)
	if err != nil {
		klog.Fatalf("failed to create plugin registry: %v", err)
		os.Exit(1)
	}

	// udev discovery looks up devices and listens for system events
	devDiscovery, err := udev.NewDiscovery(appWaitGroup)
	if err != nil {
		klog.Fatalf("failed to start udev discovery: %v", err)
		os.Exit(1)
	}
	defer devDiscovery.Close()

	domain := flags.config.DeviceDomain

	var cancel mux.CancelFunc = mux.CancelFunc(appCancel)
	for _, partConfig := range flags.config.Partitions {
		partDomain := partConfig.DomainOverride
		if partDomain == "" {
			partDomain = domain
		}
		cancel = mux.ChainCancelFunc(
			plugin.NewScatter(
				devDiscovery,
				registry,
				plugin.PartitionLabelMatcherTemplater(partDomain, partConfig.matcher),
				plugin.PartitionLabelMatcherInstances(partDomain, partConfig.matcher, flags.config.DisableTopologyHints),
			),
			cancel,
		)
	}

	for _, netBWConfig := range flags.config.NetworkBandwidth {
		cancel = mux.ChainCancelFunc(
			plugin.NewScatter(
				devDiscovery,
				registry,
				plugin.NetBWMatcherTemplater(domain, netBWConfig.matcher),
				plugin.NetBWMatcherInstances(domain, netBWConfig.matcher, netBWConfig.MbpsPerShare),
			),
			cancel,
		)
	}

	klog.Info("Starting /healthz server on port :8080")
	go func() {
		http.HandleFunc("/healthz", registry.Healthz)
		if err := http.ListenAndServe(":8080", nil); err != nil {
			klog.Fatalf("failed to start /healthz server: %v", err)
		}
	}()

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	for signal := range sigs {
		switch signal {
		case syscall.SIGINT, syscall.SIGTERM:
			klog.Infof("Received signal %q, shutting down", signal.String())
			return
		}
	}
}

type configSource interface {
	String() string
	open() (io.Reader, func() error, error)
}

type fileConfigSource struct {
	path string
}

func (fcs *fileConfigSource) open() (io.Reader, func() error, error) {
	file, err := os.Open(fcs.path)
	if err != nil {
		return nil, nil, err
	}
	return file, file.Close, nil
}

func (fcs *fileConfigSource) String() string {
	return "file:" + fcs.path
}

type envConfigSource struct {
	variable string
}

func (ecs *envConfigSource) open() (io.Reader, func() error, error) {
	data := os.Getenv(ecs.variable)
	if data == "" {
		return nil, nil, fmt.Errorf("config: environment variable %s is not set", ecs.variable)
	}
	return strings.NewReader(data), func() error { return nil }, nil
}

func (ecs *envConfigSource) String() string {
	return "env:" + ecs.variable
}

type stdinConfigSource struct{}

func (scs *stdinConfigSource) open() (io.Reader, func() error, error) {
	return os.Stdin, func() error { return nil }, nil
}

func (scs *stdinConfigSource) String() string {
	return "stdin"
}

type ConfigFlag struct {
	configSource
}

func (cf *ConfigFlag) Set(value string) error {
	if strings.HasPrefix(value, "file:") {
		cf.configSource = &fileConfigSource{path: strings.TrimPrefix(value, "file:")}
	} else if strings.HasPrefix(value, "env:") {
		cf.configSource = &envConfigSource{variable: strings.TrimPrefix(value, "env:")}
	} else if strings.HasPrefix(value, "stdin") {
		cf.configSource = &stdinConfigSource{}
	} else {
		return fmt.Errorf("invalid config source: %s", value)
	}

	return nil
}

func (cf *ConfigFlag) String() string {
	if cf.configSource == nil {
		return ""
	}
	return cf.configSource.String()
}

type FlagValues struct {
	Config ConfigFlag

	config *Config
}

func initFlags() FlagValues {
	values := FlagValues{}
	flags := flag.NewFlagSet("udev-manager", flag.ExitOnError)
	klog.InitFlags(flags)
	flags.Var(&values.Config, "config", `configuration source (in form "file:<path>", "env:<ENV_VARIABLE>" or "stdin")`)
	flags.Parse(os.Args[1:])
	if values.Config.configSource == nil {
		flags.Output().Write([]byte("config flag is required\n"))
		flags.Usage()
		os.Exit(2)
	}
	configReader, configCloser, err := values.Config.open()
	if err != nil {
		klog.Fatalf("failed to open --config %q: %v", values.Config.String(), err)
		os.Exit(1)
	}
	defer configCloser()

	config, err := parseConfig(configReader)
	if err != nil {
		klog.Fatalf("failed to parse --config %q: %v", values.Config.String(), err)
		os.Exit(1)
	}

	values.config = config

	return values
}

var (
	deviceDomainRegex = regexp.MustCompile(`^([a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?\.)+[a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?$`)
)

type PartitionsConfig struct {
	Matcher        string `yaml:"matcher"`          // matcher should be a valid regular expression
	DomainOverride string `yaml:"domain,omitempty"` // optional override for the domain

	matcher *regexp.Regexp // compiled matcher if the config is valid
}

func (pc *PartitionsConfig) validate() error {
	if pc.DomainOverride != "" {
		if !deviceDomainRegex.MatchString(pc.DomainOverride) {
			return fmt.Errorf(".domain: %q must be a valid domain name", pc.DomainOverride)
		}
	}
	// Compile the regular expression
	matcher, err := regexp.Compile(pc.Matcher)
	if err != nil {
		return fmt.Errorf(".matcher: %q must be a valid regexp: %w", pc.Matcher, err)
	}
	if matcher.NumSubexp() > 1 {
		return fmt.Errorf(".matcher: %q must have at most one capturing group", pc.Matcher)
	}
	pc.matcher = matcher
	return nil
}

type HostDevConfig struct {
	Matcher string `yaml:"matcher"` // matcher should be a valid regular expression
	Prefix  string `yaml:"prefix"`

	matcher *regexp.Regexp // compiled matcher if the config is valid
}

func (hc *HostDevConfig) validate() error {
	// Compile the regular expression
	matcher, err := regexp.Compile(hc.Matcher)
	if err != nil {
		return fmt.Errorf(".matcher: %q must be a valid regexp: %w", hc.Matcher, err)
	}
	hc.matcher = matcher
	return nil
}

type NetBWConfig struct {
	Matcher      string `yaml:"matcher"` // matcher should be a valid regular expression
	MbpsPerShare uint   `yaml:"mbpsPerShare"`

	matcher *regexp.Regexp // compiled matcher if the config is valid
}

func (nbc *NetBWConfig) validate() error {
	// Compile the regular expression
	matcher, err := regexp.Compile(nbc.Matcher)
	if err != nil {
		return fmt.Errorf(".matcher: %q must be a valid regexp: %w", nbc.Matcher, err)
	}
	nbc.matcher = matcher
	return nil
}

type Config struct {
	DeviceDomain         string             `yaml:"domain"`
	DisableTopologyHints bool               `yaml:"disable_topology_hints"`
	Partitions           []PartitionsConfig `yaml:"partitions"`
	HostDevs             []HostDevConfig    `yaml:"hostdevs"`
	NetworkBandwidth     []NetBWConfig      `yaml:"networkBandwidth"`
}

func (c *Config) validate() error {
	var errs error
	// Validate the device domain
	if c.DeviceDomain == "" {
		errs = errors.Join(errs, fmt.Errorf(".domain: must be set"))
	}
	if !deviceDomainRegex.MatchString(c.DeviceDomain) {
		errs = errors.Join(errs, fmt.Errorf(".domain: %q must be a valid domain name", c.DeviceDomain))
	}

	// Validate partitions
	for i := range c.Partitions {
		if err := c.Partitions[i].validate(); err != nil {
			errs = errors.Join(errs, fmt.Errorf(".partitions[%d]%w", i, err))
		}
	}

	// Validate hostdevs
	for i := range c.HostDevs {
		if err := c.HostDevs[i].validate(); err != nil {
			errs = errors.Join(errs, fmt.Errorf(".hostdevs[%d]%w", i, err))
		}
	}

	// Validate network bandwidth
	for i := range c.NetworkBandwidth {
		if err := c.NetworkBandwidth[i].validate(); err != nil {
			errs = errors.Join(errs, fmt.Errorf(".networkBandwidth[%d]%w", i, err))
		}
	}

	return nil
}

func parseConfig(reader io.Reader) (*Config, error) {
	// Parse the config file
	decoder := yaml.NewDecoder(reader)
	config := &Config{}
	if err := decoder.Decode(config); err != nil {
		return nil, err
	}

	// Validate the config
	if err := config.validate(); err != nil {
		return nil, err
	}

	return config, nil
}

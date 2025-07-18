// Copyright Elasticsearch B.V. and/or licensed to Elasticsearch B.V. under one
// or more contributor license agreements. Licensed under the Elastic License 2.0;
// you may not use this file except in compliance with the Elastic License 2.0.

package cmd

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"go.elastic.co/apm/v2"
	"gopkg.in/yaml.v2"

	"github.com/elastic/elastic-agent-libs/transport/httpcommon"
	"github.com/elastic/elastic-agent-libs/transport/tlscommon"
	"github.com/elastic/elastic-agent/internal/pkg/agent/application/filelock"
	"github.com/elastic/elastic-agent/internal/pkg/agent/application/info"
	"github.com/elastic/elastic-agent/internal/pkg/agent/application/paths"
	"github.com/elastic/elastic-agent/internal/pkg/agent/application/secret"
	"github.com/elastic/elastic-agent/internal/pkg/agent/configuration"
	"github.com/elastic/elastic-agent/internal/pkg/agent/errors"
	"github.com/elastic/elastic-agent/internal/pkg/agent/perms"
	"github.com/elastic/elastic-agent/internal/pkg/agent/vault"
	"github.com/elastic/elastic-agent/internal/pkg/cli"
	"github.com/elastic/elastic-agent/internal/pkg/config"
	"github.com/elastic/elastic-agent/internal/pkg/core/authority"
	"github.com/elastic/elastic-agent/internal/pkg/core/backoff"
	monitoringConfig "github.com/elastic/elastic-agent/internal/pkg/core/monitoring/config"
	"github.com/elastic/elastic-agent/internal/pkg/crypto"
	"github.com/elastic/elastic-agent/internal/pkg/fleetapi"
	fleetclient "github.com/elastic/elastic-agent/internal/pkg/fleetapi/client"
	"github.com/elastic/elastic-agent/internal/pkg/release"
	"github.com/elastic/elastic-agent/internal/pkg/remote"
	"github.com/elastic/elastic-agent/pkg/control/v2/client"
	"github.com/elastic/elastic-agent/pkg/control/v2/client/wait"
	"github.com/elastic/elastic-agent/pkg/core/logger"
	"github.com/elastic/elastic-agent/pkg/core/process"
	"github.com/elastic/elastic-agent/pkg/utils"
)

const (
	maxRetriesstoreAgentInfo       = 5
	waitingForAgent                = "Waiting for Elastic Agent to start"
	waitingForFleetServer          = "Waiting for Elastic Agent to start Fleet Server"
	defaultFleetServerHost         = "0.0.0.0"
	defaultFleetServerPort         = 8220
	defaultFleetServerInternalHost = "localhost"
	defaultFleetServerInternalPort = 8221
	enrollBackoffInit              = time.Second * 5
	enrollBackoffMax               = time.Minute * 10
)

var (
	enrollDelay   = 1 * time.Second  // max delay to start enrollment
	daemonTimeout = 30 * time.Second // max amount of for communication to running Agent daemon
)

type saver interface {
	Save(io.Reader) error
}

// enrollCmd is an enroll subcommand that interacts between the Kibana API and the Agent.
type enrollCmd struct {
	log            *logger.Logger
	options        *enrollCmdOption
	client         fleetclient.Sender
	configStore    saver
	remoteConfig   remote.Config
	agentProc      *process.Info
	configPath     string
	backoffFactory func(done <-chan struct{}) backoff.Backoff

	// For testability
	daemonReloadFunc func(context.Context) error
}

// enrollCmdFleetServerOption define all the supported enrollment options for bootstrapping with Fleet Server.
type enrollCmdFleetServerOption struct {
	ConnStr               string
	ElasticsearchCA       string
	ElasticsearchCASHA256 string
	ElasticsearchInsecure bool
	ElasticsearchCert     string
	ElasticsearchCertKey  string
	ServiceToken          string
	ServiceTokenPath      string
	PolicyID              string
	Host                  string
	Port                  uint16
	InternalPort          uint16
	Cert                  string
	CertKey               string
	CertKeyPassphrasePath string
	ClientAuth            string
	Insecure              bool
	SpawnAgent            bool
	Headers               map[string]string
	Timeout               time.Duration
}

// enrollCmdOption define all the supported enrollment option.
type enrollCmdOption struct {
	URL                  string                     `yaml:"url,omitempty"`
	InternalURL          string                     `yaml:"-"`
	CAs                  []string                   `yaml:"ca,omitempty"`
	CASha256             []string                   `yaml:"ca_sha256,omitempty"`
	Certificate          string                     `yaml:"certificate,omitempty"`
	Key                  string                     `yaml:"key,omitempty"`
	KeyPassphrasePath    string                     `yaml:"key_passphrase_path,omitempty"`
	Insecure             bool                       `yaml:"insecure,omitempty"`
	ID                   string                     `yaml:"id,omitempty"`
	ReplaceToken         string                     `yaml:"replace_token,omitempty"`
	EnrollAPIKey         string                     `yaml:"enrollment_key,omitempty"`
	Staging              string                     `yaml:"staging,omitempty"`
	ProxyURL             string                     `yaml:"proxy_url,omitempty"`
	ProxyDisabled        bool                       `yaml:"proxy_disabled,omitempty"`
	ProxyHeaders         map[string]string          `yaml:"proxy_headers,omitempty"`
	DaemonTimeout        time.Duration              `yaml:"daemon_timeout,omitempty"`
	UserProvidedMetadata map[string]interface{}     `yaml:"-"`
	FixPermissions       *utils.FileOwner           `yaml:"-"`
	DelayEnroll          bool                       `yaml:"-"`
	FleetServer          enrollCmdFleetServerOption `yaml:"-"`
	SkipCreateSecret     bool                       `yaml:"-"`
	SkipDaemonRestart    bool                       `yaml:"-"`
	Tags                 []string                   `yaml:"omitempty"`
}

// remoteConfig returns the configuration used to connect the agent to a fleet process.
func (e *enrollCmdOption) remoteConfig() (remote.Config, error) {
	cfg, err := remote.NewConfigFromURL(e.URL)
	if err != nil {
		return remote.Config{}, err
	}
	if cfg.Protocol == remote.ProtocolHTTP && !e.Insecure {
		return remote.Config{}, fmt.Errorf("connection to fleet-server is insecure, strongly recommended to use a secure connection (override with --insecure)")
	}

	var tlsCfg tlscommon.Config

	// Add any SSL options from the CLI.
	if len(e.CAs) > 0 || len(e.CASha256) > 0 {
		tlsCfg.CAs = e.CAs
		tlsCfg.CASha256 = e.CASha256
	}
	if e.Insecure {
		tlsCfg.VerificationMode = tlscommon.VerifyNone
	}
	if e.Certificate != "" || e.Key != "" {
		tlsCfg.Certificate = tlscommon.CertificateConfig{
			Certificate:    e.Certificate,
			Key:            e.Key,
			PassphrasePath: e.KeyPassphrasePath,
		}
	}

	cfg.Transport.TLS = &tlsCfg

	proxySettings, err := httpcommon.NewHTTPClientProxySettings(e.ProxyURL, e.ProxyHeaders, e.ProxyDisabled)
	if err != nil {
		return remote.Config{}, err
	}

	cfg.Transport.Proxy = *proxySettings

	return cfg, nil
}

// newEnrollCmd creates a new enrollment with the given store.
func newEnrollCmd(
	log *logger.Logger,
	options *enrollCmdOption,
	configPath string,
	store saver,
	backoffFactory func(done <-chan struct{}) backoff.Backoff,
) (*enrollCmd, error) {
	if backoffFactory == nil {
		backoffFactory = func(done <-chan struct{}) backoff.Backoff {
			return backoff.NewEqualJitterBackoff(done, enrollBackoffInit, enrollBackoffMax)
		}
	}
	return &enrollCmd{
		log:              log,
		options:          options,
		configStore:      store,
		configPath:       configPath,
		daemonReloadFunc: daemonReload,
		backoffFactory:   backoffFactory,
	}, nil
}

// Execute enrolls the agent into Fleet.
func (c *enrollCmd) Execute(ctx context.Context, streams *cli.IOStreams) error {
	var err error
	defer c.stopAgent() // ensure its stopped no matter what

	span, ctx := apm.StartSpan(ctx, "enroll", "app.internal")
	defer func() {
		apm.CaptureError(ctx, err).Send()
		span.End()
	}()

	hasRoot, err := utils.HasRoot()
	if err != nil {
		return fmt.Errorf("checking if running with root/Administrator privileges: %w", err)
	}

	// Create encryption key from the agent before touching configuration
	if !c.options.SkipCreateSecret {
		opts := []vault.OptionFunc{vault.WithUnprivileged(!hasRoot)}
		if c.options.FixPermissions != nil {
			opts = append(opts, vault.WithVaultOwnership(*c.options.FixPermissions))
		}
		err = secret.CreateAgentSecret(ctx, opts...)
		if err != nil {
			return err
		}
	}

	persistentConfig, err := getPersistentConfig(c.configPath)
	if err != nil {
		return err
	}

	// localFleetServer indicates that we start our internal fleet server. Agent
	// will communicate to the internal fleet server on localhost only.
	// Connection setup should disable proxies in that case.
	localFleetServer := c.options.FleetServer.ConnStr != ""
	if localFleetServer && !c.options.DelayEnroll {
		token, err := c.fleetServerBootstrap(ctx, persistentConfig)
		if err != nil {
			return err
		}
		if c.options.EnrollAPIKey == "" && token != "" {
			c.options.EnrollAPIKey = token
		}
	}

	c.remoteConfig, err = c.options.remoteConfig()
	if err != nil {
		return errors.New(
			err, "Error",
			errors.TypeConfig,
			errors.M(errors.MetaKeyURI, c.options.URL))
	}
	if localFleetServer {
		// Ensure that the agent does not use a proxy configuration
		// when connecting to the local fleet server.
		// Note that when running fleet-server the enroll request will be sent to :8220,
		// however when the agent is running afterward requests will be sent to :8221
		c.remoteConfig.Transport.Proxy.Disable = true
	}

	c.client, err = fleetclient.NewWithConfig(c.log, c.remoteConfig)
	if err != nil {
		return errors.New(
			err, "Error",
			errors.TypeNetwork,
			errors.M(errors.MetaKeyURI, c.options.URL))
	}

	if c.options.DelayEnroll {
		if c.options.FleetServer.Host != "" {
			return errors.New("--delay-enroll cannot be used with --fleet-server-es", errors.TypeConfig)
		}
		err = c.writeDelayEnroll(streams)
		if err != nil {
			// context for error already provided in writeDelayEnroll
			return err
		}
		if c.options.FixPermissions != nil {
			err = perms.FixPermissions(paths.Top(), perms.WithOwnership(*c.options.FixPermissions))
			if err != nil {
				return errors.New(err, "failed to fix permissions")
			}
		}
		return nil
	}

	err = c.enrollWithBackoff(ctx, persistentConfig)
	if err != nil {
		return fmt.Errorf("fail to enroll: %w", err)
	}

	if c.options.FixPermissions != nil {
		err = perms.FixPermissions(paths.Top(), perms.WithOwnership(*c.options.FixPermissions))
		if err != nil {
			return errors.New(err, "failed to fix permissions")
		}
	}

	defer func() {
		if err != nil {
			fmt.Fprintf(streams.Err, "Something went wrong while enrolling the Elastic Agent: %v\n", err)
		} else {
			fmt.Fprintln(streams.Out, "Successfully enrolled the Elastic Agent.")
		}
	}()

	if c.agentProc == nil && !c.options.SkipDaemonRestart {
		if err = c.daemonReloadWithBackoff(ctx); err != nil {
			c.log.Errorf("Elastic Agent might not be running; unable to trigger restart: %v", err)
			return fmt.Errorf("could not reload agent daemon, unable to trigger restart: %w", err)
		}

		c.log.Info("Successfully triggered restart on running Elastic Agent.")
		return nil
	}

	c.log.Info("Elastic Agent has been enrolled; start Elastic Agent")
	return nil
}

func (c *enrollCmd) writeDelayEnroll(streams *cli.IOStreams) error {
	enrollPath := paths.AgentEnrollFile()
	data, err := yaml.Marshal(c.options)
	if err != nil {
		return errors.New(
			err,
			"failed to marshall enrollment options",
			errors.TypeConfig,
			errors.M("path", enrollPath))
	}
	err = os.WriteFile(enrollPath, data, 0600)
	if err != nil {
		return errors.New(
			err,
			"failed to write enrollment options file",
			errors.TypeFilesystem,
			errors.M("path", enrollPath))
	}
	fmt.Fprintf(streams.Out, "Successfully wrote %s for delayed enrollment of the Elastic Agent.\n", enrollPath)
	return nil
}

func (c *enrollCmd) fleetServerBootstrap(ctx context.Context, persistentConfig map[string]interface{}) (string, error) {
	c.log.Debug("verifying communication with running Elastic Agent daemon")
	agentRunning := true
	if c.options.FleetServer.InternalPort == 0 {
		c.options.FleetServer.InternalPort = defaultFleetServerInternalPort
	}
	_, err := getDaemonState(ctx)
	if err != nil {
		if !c.options.FleetServer.SpawnAgent {
			// wait longer to try and communicate with the Elastic Agent
			err = wait.ForAgent(ctx, c.options.DaemonTimeout)
			if err != nil {
				return "", errors.New("failed to communicate with elastic-agent daemon; is elastic-agent running?")
			}
		} else {
			agentRunning = false
		}
	}

	err = c.prepareFleetTLS()
	if err != nil {
		return "", err
	}

	agentConfig := c.createAgentConfig("", persistentConfig, c.options.FleetServer.Headers)

	//nolint:dupl // duplicate because same params are passed
	fleetConfig, err := createFleetServerBootstrapConfig(
		c.options.FleetServer.ConnStr, c.options.FleetServer.ServiceToken, c.options.FleetServer.ServiceTokenPath,
		c.options.FleetServer.PolicyID,
		c.options.FleetServer.Host, c.options.FleetServer.Port, c.options.FleetServer.InternalPort,
		c.options.FleetServer.Cert, c.options.FleetServer.CertKey, c.options.FleetServer.CertKeyPassphrasePath, c.options.FleetServer.ElasticsearchCA, c.options.FleetServer.ElasticsearchCASHA256,
		c.options.CAs, c.options.FleetServer.ClientAuth,
		c.options.FleetServer.ElasticsearchCert, c.options.FleetServer.ElasticsearchCertKey,
		c.options.FleetServer.Headers,
		c.options.ProxyURL,
		c.options.ProxyDisabled,
		c.options.ProxyHeaders,
		c.options.FleetServer.ElasticsearchInsecure,
	)
	if err != nil {
		return "", err
	}
	c.options.FleetServer.InternalPort = fleetConfig.Server.InternalPort

	configToStore := map[string]interface{}{
		"agent": agentConfig,
		"fleet": fleetConfig,
	}
	reader, err := yamlToReader(configToStore)
	if err != nil {
		return "", err
	}

	if err := safelyStoreAgentInfo(c.configStore, reader); err != nil {
		return "", err
	}

	var agentSubproc <-chan *os.ProcessState
	if agentRunning {
		// reload the already running agent
		err = c.daemonReloadWithBackoff(ctx)
		if err != nil {
			return "", errors.New(err, "failed to trigger elastic-agent daemon reload", errors.TypeApplication)
		}
	} else {
		// spawn `run` as a subprocess so enroll can perform the bootstrap process of Fleet Server
		agentSubproc, err = c.startAgent(ctx)
		if err != nil {
			return "", err
		}
	}

	token, err := waitForFleetServer(ctx, agentSubproc, c.log, c.options.FleetServer.Timeout)
	if err != nil {
		return "", errors.New(err, "fleet-server failed", errors.TypeApplication)
	}
	return token, nil
}

func (c *enrollCmd) prepareFleetTLS() error {
	host := c.options.FleetServer.Host
	if host == "" {
		host = defaultFleetServerInternalHost
	}
	port := c.options.FleetServer.Port
	if port == 0 {
		port = defaultFleetServerPort
	}
	if c.options.FleetServer.Cert != "" && c.options.FleetServer.CertKey == "" {
		return errors.New("certificate private key is required when certificate provided")
	}
	if c.options.FleetServer.CertKey != "" && c.options.FleetServer.Cert == "" {
		return errors.New("certificate is required when certificate private key is provided")
	}
	if c.options.FleetServer.Cert == "" && c.options.FleetServer.CertKey == "" {
		if c.options.FleetServer.Insecure {
			// running insecure, force the binding to localhost (unless specified)
			if c.options.FleetServer.Host == "" {
				c.options.FleetServer.Host = defaultFleetServerInternalHost
			}
			c.options.URL = "http://" + net.JoinHostPort(host, strconv.Itoa(int(port)))
			c.options.Insecure = true
			return nil
		}

		c.log.Info("Generating self-signed certificate for Fleet Server")
		hostname, err := os.Hostname()
		if err != nil {
			return err
		}
		ca, err := authority.NewCA()
		if err != nil {
			return err
		}
		pair, err := ca.GeneratePairWithName(hostname)
		if err != nil {
			return err
		}
		c.options.FleetServer.Cert = string(pair.Crt)
		c.options.FleetServer.CertKey = string(pair.Key)
		c.options.URL = "https://" + net.JoinHostPort(hostname, strconv.Itoa(int(port)))
		c.options.CAs = []string{string(ca.Crt())}
	}
	// running with custom Cert and CertKey; URL is required to be set
	if c.options.URL == "" {
		return errors.New("url is required when a certificate is provided")
	}

	if c.options.FleetServer.InternalPort > 0 {
		if c.options.FleetServer.InternalPort != defaultFleetServerInternalPort {
			c.log.Warnf("Internal endpoint configured to: %d. Changing this value is not supported.", c.options.FleetServer.InternalPort)
		}
		c.options.InternalURL = net.JoinHostPort(defaultFleetServerInternalHost, strconv.Itoa(int(c.options.FleetServer.InternalPort)))
	}

	return nil
}

const (
	daemonReloadInitBackoff = time.Second
	daemonReloadMaxBackoff  = time.Minute
	daemonReloadRetries     = 5
)

func (c *enrollCmd) daemonReloadWithBackoff(ctx context.Context) error {
	backExp := backoff.NewExpBackoff(ctx.Done(), daemonReloadInitBackoff, daemonReloadMaxBackoff)

	var lastErr error
	for i := 0; i < daemonReloadRetries; i++ {
		attempt := i

		c.log.Infof("Restarting agent daemon, attempt %d", attempt)
		err := c.daemonReloadFunc(ctx)
		if err == nil {
			return nil
		}

		// If the context was cancelled, return early
		if errors.Is(err, context.DeadlineExceeded) ||
			errors.Is(err, context.Canceled) {
			return fmt.Errorf("could not reload daemon after %d retries: %w",
				attempt, err)
		}
		lastErr = err

		c.log.Errorf("Restart attempt %d failed: '%s'. Waiting for %s", attempt, err, backExp.NextWait().String())
		// backoff Wait returns false if context.Done()
		if !backExp.Wait() {
			return ctx.Err()
		}
	}

	return fmt.Errorf("could not reload agent's daemon, all retries failed. Last error: %w", lastErr)
}

func daemonReload(ctx context.Context) error {
	daemon := client.New()
	err := daemon.Connect(ctx)
	if err != nil {
		return err
	}
	defer daemon.Disconnect()
	return daemon.Restart(ctx)
}

func (c *enrollCmd) enrollWithBackoff(ctx context.Context, persistentConfig map[string]interface{}) error {
	delay(ctx, enrollDelay)

	c.log.Infof("Starting enrollment to URL: %s", c.client.URI())
	err := c.enroll(ctx, persistentConfig)
	if err == nil {
		return nil
	}

	c.log.Infof("1st enrollment attempt failed, retrying enrolling to URL: %s with exponential backoff (init %s, max %s)", c.client.URI(), enrollBackoffInit, enrollBackoffMax)

	signal := make(chan struct{})
	defer close(signal)
	backExp := c.backoffFactory(signal)

RETRYLOOP:
	for {
		switch {
		case errors.Is(err, fleetapi.ErrTooManyRequests):
			c.log.Warn("Too many requests on the remote server, will retry in a moment.")
		case errors.Is(err, fleetapi.ErrConnRefused):
			c.log.Warn("Remote server is not ready to accept connections(Connection Refused), will retry in a moment.")
		case errors.Is(err, fleetapi.ErrTemporaryServerError):
			c.log.Warnf("Remote server failed to handle the request(%s), will retry in a moment.", err.Error())
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded), err == nil:
			break RETRYLOOP
		case err != nil:
			c.log.Warnf("Error detected: %s, will retry in a moment.", err.Error())
		}
		if !backExp.Wait() {
			break RETRYLOOP
		}
		c.log.Infof("Retrying enrollment to URL: %s", c.client.URI())
		err = c.enroll(ctx, persistentConfig)
	}

	return err
}

func (c *enrollCmd) enroll(ctx context.Context, persistentConfig map[string]interface{}) error {
	cmd := fleetapi.NewEnrollCmd(c.client)

	metadata, err := info.Metadata(ctx, c.log)
	if err != nil {
		return errors.New(err, "acquiring metadata failed")
	}

	// Automatically add the namespace as a tag when installed into a namepsace.
	// Ensures the development agent is differentiated from others when on the same host.
	if namespace := paths.InstallNamespace(); namespace != "" {
		c.options.Tags = append(c.options.Tags, namespace)
	}

	r := &fleetapi.EnrollRequest{
		EnrollAPIKey: c.options.EnrollAPIKey,
		Type:         fleetapi.PermanentEnroll,
		ID:           c.options.ID,
		ReplaceToken: c.options.ReplaceToken,
		Metadata: fleetapi.Metadata{
			Local:        metadata,
			UserProvided: c.options.UserProvidedMetadata,
			Tags:         cleanTags(c.options.Tags),
		},
	}

	resp, err := cmd.Execute(ctx, r)
	if err != nil {
		return fmt.Errorf("failed to execute request to fleet-server: %w", err)
	}

	fleetConfig, err := createFleetConfigFromEnroll(resp.Item.AccessAPIKey, c.options.EnrollAPIKey, c.options.ReplaceToken, c.remoteConfig)
	if err != nil {
		return err
	}

	agentConfig := c.createAgentConfig(resp.Item.ID, persistentConfig, c.options.FleetServer.Headers)

	localFleetServer := c.options.FleetServer.ConnStr != ""
	if localFleetServer {
		//nolint:dupl // not duplicates, just similar params are passed
		serverConfig, err := createFleetServerBootstrapConfig(
			c.options.FleetServer.ConnStr, c.options.FleetServer.ServiceToken, c.options.FleetServer.ServiceTokenPath,
			c.options.FleetServer.PolicyID,
			c.options.FleetServer.Host, c.options.FleetServer.Port, c.options.FleetServer.InternalPort,
			c.options.FleetServer.Cert, c.options.FleetServer.CertKey, c.options.FleetServer.CertKeyPassphrasePath, c.options.FleetServer.ElasticsearchCA, c.options.FleetServer.ElasticsearchCASHA256,
			c.options.CAs, c.options.FleetServer.ClientAuth,
			c.options.FleetServer.ElasticsearchCert, c.options.FleetServer.ElasticsearchCertKey,
			c.options.FleetServer.Headers,
			c.options.ProxyURL, c.options.ProxyDisabled, c.options.ProxyHeaders,
			c.options.FleetServer.ElasticsearchInsecure,
		)
		if err != nil {
			return fmt.Errorf(
				"failed creating fleet-server bootstrap config: %w", err)
		}

		// no longer need bootstrap at this point
		serverConfig.Server.Bootstrap = false
		fleetConfig.Server = serverConfig.Server
		// use internal URL for future requests
		if c.options.InternalURL != "" {
			fleetConfig.Client.Host = c.options.InternalURL
			// fleet-server will bind the internal listenter to localhost:8221
			// InternalURL is localhost:8221, however cert uses $HOSTNAME, so we need to disable hostname verification.
			fleetConfig.Client.Transport.TLS.VerificationMode = tlscommon.VerifyCertificate
		}
	}

	configToStore := map[string]interface{}{
		"fleet": fleetConfig,
		"agent": agentConfig,
	}

	reader, err := yamlToReader(configToStore)
	if err != nil {
		return fmt.Errorf("yamlToReader failed: %w", err)
	}

	if err := safelyStoreAgentInfo(c.configStore, reader); err != nil {
		return fmt.Errorf("failed to store agent config: %w", err)
	}

	// clear action store
	// fail only if file exists and there was a failure
	if err := os.Remove(paths.AgentActionStoreFile()); !os.IsNotExist(err) {
		return err
	}

	// clear action store
	// fail only if file exists and there was a failure
	if err := os.Remove(paths.AgentStateStoreFile()); !os.IsNotExist(err) {
		return err
	}

	return nil
}

func (c *enrollCmd) startAgent(ctx context.Context) (<-chan *os.ProcessState, error) {
	cmd, err := os.Executable()
	if err != nil {
		return nil, err
	}
	c.log.Info("Spawning Elastic Agent daemon as a subprocess to complete bootstrap process.")
	args := []string{
		"run", "-e", "-c", paths.ConfigFile(),
		"--path.home", paths.Top(), "--path.config", paths.Config(),
		"--path.logs", paths.Logs(), "--path.socket", paths.ControlSocket(),
	}
	if paths.Downloads() != "" {
		args = append(args, "--path.downloads", paths.Downloads())
	}
	if !paths.IsVersionHome() {
		args = append(args, "--path.home.unversioned")
	}
	proc, err := process.Start(
		cmd,
		process.WithContext(ctx),
		process.WithArgs(args),
		process.WithCmdOptions(func(c *exec.Cmd) error {
			c.Stdout = os.Stdout
			c.Stderr = os.Stderr
			return nil
		}))
	if err != nil {
		return nil, err
	}
	resChan := make(chan *os.ProcessState)
	go func() {
		procState, _ := proc.Process.Wait()
		resChan <- procState
	}()
	c.agentProc = proc
	return resChan, nil
}

func (c *enrollCmd) stopAgent() {
	if c.agentProc != nil {
		_ = c.agentProc.StopWait()
		c.agentProc = nil
	}
}

func yamlToReader(in interface{}) (io.Reader, error) {
	data, err := yaml.Marshal(in)
	if err != nil {
		return nil, errors.New(err, "could not marshal to YAML")
	}
	return bytes.NewReader(data), nil
}

func delay(ctx context.Context, d time.Duration) {
	t := time.NewTimer(rand.N(d))
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

func getDaemonState(ctx context.Context) (*client.AgentState, error) {
	ctx, cancel := context.WithTimeout(ctx, daemonTimeout)
	defer cancel()
	daemon := client.New()
	err := daemon.Connect(ctx)
	if err != nil {
		return nil, err
	}
	defer daemon.Disconnect()
	return daemon.State(ctx)
}

type waitResult struct {
	enrollmentToken string
	err             error
}

func waitForFleetServer(ctx context.Context, agentSubproc <-chan *os.ProcessState, log *logger.Logger, timeout time.Duration) (string, error) {
	if timeout == 0 {
		timeout = 2 * time.Minute
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	maxBackoff := timeout
	if maxBackoff <= 0 {
		// indefinite timeout
		maxBackoff = 10 * time.Minute
	}

	resChan := make(chan waitResult)
	innerCtx, innerCancel := context.WithCancel(context.Background())
	defer innerCancel()
	go func() {
		msg := ""
		msgCount := 0
		backExp := expBackoffWithContext(innerCtx, 1*time.Second, maxBackoff)

		for {
			// if the timeout is reached, no response was sent on `res`, therefore
			// send an error
			if !backExp.Wait() {
				resChan <- waitResult{err: fmt.Errorf(
					"timed out waiting for Fleet Server to start after %s",
					timeout)}
			}

			state, err := getDaemonState(innerCtx)
			if errors.Is(err, context.Canceled) {
				resChan <- waitResult{err: err}
				return
			}
			if err != nil {
				log.Debugf("%s: %s", waitingForAgent, err)
				if msg != waitingForAgent {
					msg = waitingForAgent
					msgCount = 0
					log.Info(waitingForAgent)
				} else {
					msgCount++
					if msgCount > 5 {
						msgCount = 0
						log.Infof("%s: %s", waitingForAgent, err)
					}
				}
				continue
			}
			unit := getCompUnitFromStatus(state, "fleet-server")
			if unit == nil {
				err = errors.New("no fleet-server application running")
				log.Debugf("%s: %s", waitingForFleetServer, err)
				if msg != waitingForFleetServer {
					msg = waitingForFleetServer
					msgCount = 0
					log.Info(waitingForFleetServer)
				} else {
					msgCount++
					if msgCount > 5 {
						msgCount = 0
						log.Infof("%s: %s", waitingForFleetServer, err)
					}
				}
				continue
			}
			log.Debugf("%s: %s - %s", waitingForFleetServer, unit.State, unit.Message)
			if unit.State == client.Degraded || unit.State == client.Healthy {
				// app has started and is running
				if unit.Message != "" {
					log.Infof("Fleet Server - %s", unit.Message)
				}
				// extract the enrollment token from the status payload
				token := ""
				if unit.Payload != nil {
					if enrollToken, ok := unit.Payload["enrollment_token"]; ok {
						if tokenStr, ok := enrollToken.(string); ok {
							token = tokenStr
						}
					}
				}
				resChan <- waitResult{enrollmentToken: token}
				break
			}
			if unit.Message != "" {
				appMsg := fmt.Sprintf("Fleet Server - %s", unit.Message)
				if msg != appMsg {
					msg = appMsg
					msgCount = 0
					log.Info(appMsg)
				} else {
					msgCount++
					if msgCount > 5 {
						msgCount = 0
						log.Info(appMsg)
					}
				}
			}
		}
	}()

	var res waitResult
	if agentSubproc == nil {
		select {
		case <-ctx.Done():
			innerCancel()
			res = <-resChan
		case res = <-resChan:
		}
	} else {
		select {
		case ps := <-agentSubproc:
			res = waitResult{err: fmt.Errorf("spawned Elastic Agent exited unexpectedly: %s", ps)}
		case <-ctx.Done():
			innerCancel()
			res = <-resChan
		case res = <-resChan:
		}
	}

	if res.err != nil {
		return "", res.err
	}
	return res.enrollmentToken, nil
}

func getCompUnitFromStatus(state *client.AgentState, name string) *client.ComponentUnitState {
	for _, comp := range state.Components {
		if comp.Name == name {
			for _, unit := range comp.Units {
				if unit.UnitType == client.UnitTypeInput {
					return &unit
				}
			}
		}
	}
	return nil
}

func safelyStoreAgentInfo(s saver, reader io.Reader) error {
	var err error
	signal := make(chan struct{})
	backExp := backoff.NewExpBackoff(signal, 100*time.Millisecond, 3*time.Second)

	for i := 0; i <= maxRetriesstoreAgentInfo; i++ {
		backExp.Wait()
		err = storeAgentInfo(s, reader)
		if !errors.Is(err, filelock.ErrAppAlreadyRunning) {
			break
		}
	}

	close(signal)
	return err
}

func storeAgentInfo(s saver, reader io.Reader) error {
	fileLock := paths.AgentConfigFileLock()
	if err := fileLock.TryLock(); err != nil {
		return err
	}
	defer func() {
		_ = fileLock.Unlock()
	}()

	if err := s.Save(reader); err != nil {
		return errors.New(err, "could not save enrollment information", errors.TypeFilesystem)
	}

	return nil
}

func createFleetServerBootstrapConfig(
	connStr, serviceToken, serviceTokenPath, policyID, host string,
	port uint16, internalPort uint16,
	cert, key, passphrasePath, esCA, esCASHA256 string,
	cas []string, clientAuth string,
	esClientCert, esClientCertKey string,
	headers map[string]string,
	proxyURL string,
	proxyDisabled bool,
	proxyHeaders map[string]string,
	insecure bool,
) (*configuration.FleetAgentConfig, error) {
	localFleetServer := connStr != ""

	es, err := configuration.ElasticsearchFromConnStr(connStr, serviceToken, serviceTokenPath, insecure)
	if err != nil {
		return nil, err
	}
	if esCA != "" {
		if es.TLS == nil {
			es.TLS = &tlscommon.Config{
				CAs: []string{esCA},
			}
		} else {
			es.TLS.CAs = []string{esCA}
		}
	}
	if esCASHA256 != "" {
		if es.TLS == nil {
			es.TLS = &tlscommon.Config{
				CATrustedFingerprint: esCASHA256,
			}
		} else {
			es.TLS.CATrustedFingerprint = esCASHA256
		}
	}
	if esClientCert != "" || esClientCertKey != "" {
		if es.TLS == nil {
			es.TLS = &tlscommon.Config{}
		}

		es.TLS.Certificate = tlscommon.CertificateConfig{
			Certificate: esClientCert,
			Key:         esClientCertKey,
		}
	}
	if host == "" {
		host = defaultFleetServerHost
	}
	if port == 0 {
		port = defaultFleetServerPort
	}
	if internalPort == 0 {
		internalPort = defaultFleetServerInternalPort
	}
	if len(headers) > 0 {
		if es.Headers == nil {
			es.Headers = make(map[string]string)
		}
		// overwrites previously set headers
		for k, v := range headers {
			es.Headers[k] = v
		}
	}
	es.ProxyURL = proxyURL
	es.ProxyDisable = proxyDisabled
	es.ProxyHeaders = proxyHeaders

	cfg := configuration.DefaultFleetAgentConfig()
	cfg.Enabled = true
	cfg.Server = &configuration.FleetServerConfig{
		Bootstrap: true,
		Output: configuration.FleetServerOutputConfig{
			Elasticsearch: es,
		},
		Host: host,
		Port: port,
	}

	if policyID != "" {
		cfg.Server.Policy = &configuration.FleetServerPolicyConfig{ID: policyID}
	}
	if cert != "" || key != "" {
		cfg.Server.TLS = &tlscommon.ServerConfig{
			Certificate: tlscommon.CertificateConfig{
				Certificate:    cert,
				Key:            key,
				PassphrasePath: passphrasePath,
			},
		}
		if insecure {
			cfg.Server.TLS.VerificationMode = tlscommon.VerifyNone
		}

		cfg.Server.TLS.CAs = cas

		var cAuth tlscommon.TLSClientAuth
		cfg.Server.TLS.ClientAuth = &cAuth
		if err := cfg.Server.TLS.ClientAuth.Unpack(clientAuth); err != nil {
			return nil, errors.New(err, "failed to unpack --fleet-server-client-auth", errors.TypeConfig)
		}
	}

	if localFleetServer {
		cfg.Client.Transport.Proxy.Disable = true
		cfg.Server.InternalPort = internalPort
	}

	if err := cfg.Valid(); err != nil {
		return nil, errors.New(err, "invalid enrollment options", errors.TypeConfig)
	}
	return cfg, nil
}

func fleetHashToken(token string) (string, error) {
	enrollmentHashBytes, err := crypto.GeneratePBKDF2FromPassword([]byte(token))
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(enrollmentHashBytes), nil
}

func createFleetConfigFromEnroll(accessAPIKey string, enrollmentToken string, replaceToken string, cli remote.Config) (*configuration.FleetAgentConfig, error) {
	var err error
	cfg := configuration.DefaultFleetAgentConfig()
	cfg.Enabled = true
	cfg.AccessAPIKey = accessAPIKey
	cfg.Client = cli
	cfg.EnrollmentTokenHash, err = fleetHashToken(enrollmentToken)
	if err != nil {
		return nil, errors.New(err, "failed to generate enrollment hash", errors.TypeConfig)
	}

	// Hash replaceToken if provided; it is not expected to be provided when an Agent
	// is being enrolled for the very first time. Hashing an empty replaceToken with the
	// FIPS-capable build of Elastic Agent results in an "invalid key length" error from
	// OpenSSL's FIPS provider.
	if replaceToken != "" {
		cfg.ReplaceTokenHash, err = fleetHashToken(replaceToken)
		if err != nil {
			return nil, errors.New(err, "failed to generate replace token hash", errors.TypeConfig)
		}
	}

	if err := cfg.Valid(); err != nil {
		return nil, errors.New(err, "invalid enrollment options", errors.TypeConfig)
	}
	return cfg, nil
}

func (c *enrollCmd) createAgentConfig(agentID string, pc map[string]interface{}, headers map[string]string) map[string]interface{} {
	agentConfig := map[string]interface{}{
		"id": agentID,
	}

	if len(headers) > 0 {
		agentConfig["headers"] = headers
	}

	if c.options.Staging != "" {
		staging := fmt.Sprintf("https://staging.elastic.co/%s-%s/downloads/", release.Version(), c.options.Staging[:8])
		agentConfig["download"] = map[string]interface{}{
			"sourceURI": staging,
		}
	}

	for k, v := range pc {
		agentConfig[k] = v
	}

	return agentConfig
}

func getPersistentConfig(pathConfigFile string) (map[string]interface{}, error) {
	persistentMap := make(map[string]interface{})
	rawConfig, err := config.LoadFile(pathConfigFile)
	if os.IsNotExist(err) {
		return persistentMap, nil
	}
	if err != nil {
		return nil, errors.New(err,
			fmt.Sprintf("could not read configuration file %s", pathConfigFile),
			errors.TypeFilesystem,
			errors.M(errors.MetaKeyPath, pathConfigFile))
	}

	pc := &struct {
		Headers        map[string]string                      `json:"agent.headers,omitempty" yaml:"agent.headers,omitempty" config:"agent.headers,omitempty"`
		LogLevel       string                                 `json:"agent.logging.level,omitempty" yaml:"agent.logging.level,omitempty" config:"agent.logging.level,omitempty"`
		MonitoringHTTP *monitoringConfig.MonitoringHTTPConfig `json:"agent.monitoring.http,omitempty" yaml:"agent.monitoring.http,omitempty" config:"agent.monitoring.http,omitempty"`
	}{
		MonitoringHTTP: monitoringConfig.DefaultConfig().HTTP,
	}

	if err := rawConfig.UnpackTo(&pc); err != nil {
		return nil, err
	}

	if pc.LogLevel != "" {
		persistentMap["logging.level"] = pc.LogLevel
	}

	if pc.MonitoringHTTP != nil {
		persistentMap["monitoring.http"] = pc.MonitoringHTTP
	}

	return persistentMap, nil
}

func expBackoffWithContext(ctx context.Context, init, max time.Duration) backoff.Backoff {
	signal := make(chan struct{})
	bo := backoff.NewExpBackoff(signal, init, max)
	go func() {
		<-ctx.Done()
		close(signal)
	}()
	return bo
}

func cleanTags(tags []string) []string {
	var r []string
	// Create a map to store unique elements
	seen := make(map[string]bool)
	for _, str := range tags {
		tag := strings.TrimSpace(str)
		if tag != "" {
			if _, ok := seen[tag]; !ok {
				seen[tag] = true
				r = append(r, tag)
			}
		}
	}
	return r
}

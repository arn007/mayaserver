package cmd

import (
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"sort"

	"strings"
	"syscall"
	"time"

	"github.com/openebs/mayaserver/server"

	"github.com/hashicorp/go-syslog"
	"github.com/hashicorp/logutils"
	"github.com/mitchellh/cli"
	"github.com/openebs/mayaserver/util/flag-helpers"
	"github.com/openebs/mayaserver/util/gated-writer"
)

// gracefulTimeout controls how long we wait before forcefully terminating
const gracefulTimeout = 5 * time.Second

// UpCommand is a cli implementation that runs a Maya server.
// The command will not end unless a shutdown message is sent on the
// ShutdownCh. If two messages are sent on the ShutdownCh it will forcibly
// exit.
type UpCommand struct {
	Revision          string
	Version           string
	VersionPrerelease string
	Ui                cli.Ui
	ShutdownCh        <-chan struct{}

	args       []string
	maya       *server.MayaServer
	httpServer *server.HTTPServer
	logFilter  *logutils.LevelFilter
	logOutput  io.Writer
}

func (c *UpCommand) readMayaConfig() *server.MayaConfig {
	var configPath []string

	// Make a new, empty config.
	cmdConfig := &server.MayaConfig{
		Ports: &server.Ports{},
	}

	flags := flag.NewFlagSet("up", flag.ContinueOnError)
	flags.Usage = func() { c.Ui.Error(c.Help()) }

	// options
	flags.Var((*flaghelper.StringFlag)(&configPath), "config", "config")
	flags.StringVar(&cmdConfig.BindAddr, "bind", "", "")
	flags.StringVar(&cmdConfig.Region, "region", "", "")
	flags.StringVar(&cmdConfig.DataDir, "data-dir", "", "")
	flags.StringVar(&cmdConfig.Datacenter, "dc", "", "")
	flags.StringVar(&cmdConfig.LogLevel, "log-level", "", "")

	if err := flags.Parse(c.args); err != nil {
		return nil
	}

	// Load the configuration
	mconfig := server.DefaultMayaConfig()

	for _, path := range configPath {
		current, err := server.LoadMayaConfig(path)
		if err != nil {
			c.Ui.Error(fmt.Sprintf(
				"Error loading configuration from %s: %s", path, err))
			return nil
		}

		// The user asked us to load some config here but we didn't find any,
		// so we'll complain but continue.
		if current == nil || reflect.DeepEqual(current, &server.MayaConfig{}) {
			c.Ui.Warn(fmt.Sprintf("No configuration loaded from %s", path))
		}

		if mconfig == nil {
			mconfig = current
		} else {
			mconfig = mconfig.Merge(current)
		}
	}

	// Merge any CLI options over config file options
	mconfig = mconfig.Merge(cmdConfig)

	// Set the version info
	mconfig.Revision = c.Revision
	mconfig.Version = c.Version
	mconfig.VersionPrerelease = c.VersionPrerelease

	// Normalize binds, ports, addresses, and advertise
	if err := mconfig.NormalizeAddrs(); err != nil {
		c.Ui.Error(err.Error())
		return nil
	}

	// Verify the paths are absolute.
	dirs := map[string]string{
		"data-dir": mconfig.DataDir,
	}
	for k, dir := range dirs {
		if dir == "" {
			continue
		}

		if !filepath.IsAbs(dir) {
			c.Ui.Error(fmt.Sprintf("%s must be given as an absolute path: got %v", k, dir))
			return nil
		}
	}

	return mconfig
}

// setupLoggers is used to setup the logGate, logWriter, and our logOutput
func (c *UpCommand) setupLoggers(mconfig *server.MayaConfig) (*gatedwriter.Writer, *server.LogWriter, io.Writer) {
	// Setup logging. First create the gated log writer, which will
	// store logs until we're ready to show them. Then create the level
	// filter, filtering logs of the specified level.
	logGate := &gatedwriter.Writer{
		Writer: &cli.UiWriter{Ui: c.Ui},
	}

	c.logFilter = server.LevelFilter()
	c.logFilter.MinLevel = logutils.LogLevel(strings.ToUpper(mconfig.LogLevel))
	c.logFilter.Writer = logGate
	if !server.ValidateLevelFilter(c.logFilter.MinLevel, c.logFilter) {
		c.Ui.Error(fmt.Sprintf(
			"Invalid log level: %s. Valid log levels are: %v",
			c.logFilter.MinLevel, c.logFilter.Levels))
		return nil, nil, nil
	}

	// Check if syslog is enabled
	var syslog io.Writer
	if mconfig.EnableSyslog {
		l, err := gsyslog.NewLogger(gsyslog.LOG_NOTICE, mconfig.SyslogFacility, "mayaserver")
		if err != nil {
			c.Ui.Error(fmt.Sprintf("Syslog setup failed: %v", err))
			return nil, nil, nil
		}
		syslog = &server.SyslogWrapper{l, c.logFilter}
	}

	// Create a log writer, and wrap a logOutput around it
	logWriter := server.NewLogWriter(512)
	var logOutput io.Writer
	if syslog != nil {
		logOutput = io.MultiWriter(c.logFilter, logWriter, syslog)
	} else {
		logOutput = io.MultiWriter(c.logFilter, logWriter)
	}
	c.logOutput = logOutput
	log.SetOutput(logOutput)
	return logGate, logWriter, logOutput
}

// setupMayaServer is used to start Maya server
func (c *UpCommand) setupMayaServer(mconfig *server.MayaConfig, logOutput io.Writer) error {
	c.Ui.Output("Starting Maya server ...")
	maya, err := server.NewMayaServer(mconfig, logOutput)
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error starting Maya server: %s", err))
		return err
	}
	c.maya = maya

	// Setup the HTTP server
	http, err := server.NewHTTPServer(maya, mconfig, logOutput)
	if err != nil {
		maya.Shutdown()
		c.Ui.Error(fmt.Sprintf("Error starting http server: %s", err))
		return err
	}
	c.httpServer = http

	return nil
}

func (c *UpCommand) Run(args []string) int {
	c.Ui = &cli.PrefixedUi{
		OutputPrefix: "==> ",
		InfoPrefix:   "    ",
		ErrorPrefix:  "==> ",
		Ui:           c.Ui,
	}

	// Parse our configs
	c.args = args
	mconfig := c.readMayaConfig()
	if mconfig == nil {
		return 1
	}

	// Setup the log outputs
	logGate, _, logOutput := c.setupLoggers(mconfig)
	if logGate == nil {
		return 1
	}

	// Log config files
	if len(mconfig.Files) > 0 {
		c.Ui.Info(fmt.Sprintf("Loaded configuration from %s", strings.Join(mconfig.Files, ", ")))
	} else {
		c.Ui.Info("No configuration files loaded")
	}

	// Setup Maya server
	if err := c.setupMayaServer(mconfig, logOutput); err != nil {
		return 1
	}
	defer c.maya.Shutdown()

	// Check and shut down at the end
	defer func() {
		if c.httpServer != nil {
			c.httpServer.Shutdown()
		}
	}()

	// Compile Maya server information for output later
	info := make(map[string]string)
	info["version"] = fmt.Sprintf("%s%s", mconfig.Version, mconfig.VersionPrerelease)
	info["log level"] = mconfig.LogLevel
	info["region"] = fmt.Sprintf("%s (DC: %s)", mconfig.Region, mconfig.Datacenter)

	// Sort the keys for output
	infoKeys := make([]string, 0, len(info))
	for key := range info {
		infoKeys = append(infoKeys, key)
	}
	sort.Strings(infoKeys)

	// Maya server configuration output
	padding := 18
	c.Ui.Output("Maya server configuration:\n")
	for _, k := range infoKeys {
		c.Ui.Info(fmt.Sprintf(
			"%s%s: %s",
			strings.Repeat(" ", padding-len(k)),
			strings.Title(k),
			info[k]))
	}
	c.Ui.Output("")

	// Output the header that the server has started
	c.Ui.Output("Maya server started! Log data will stream in below:\n")

	// Enable log streaming
	logGate.Flush()

	// Wait for exit
	return c.handleSignals(mconfig)
}

// handleSignals blocks until we get an exit-causing signal
func (c *UpCommand) handleSignals(mconfig *server.MayaConfig) int {
	signalCh := make(chan os.Signal, 4)
	signal.Notify(signalCh, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGPIPE)

	// Wait for a signal
WAIT:
	var sig os.Signal
	select {
	case s := <-signalCh:
		sig = s
	case <-c.ShutdownCh:
		sig = os.Interrupt
	}
	c.Ui.Output(fmt.Sprintf("Caught signal: %v", sig))

	// Skip any SIGPIPE signal (See issue #1798)
	if sig == syscall.SIGPIPE {
		goto WAIT
	}

	// Check if this is a SIGHUP
	if sig == syscall.SIGHUP {
		if conf := c.handleReload(mconfig); conf != nil {
			*mconfig = *conf
		}
		goto WAIT
	}

	// Check if we should do a graceful leave
	graceful := false
	if sig == os.Interrupt && mconfig.LeaveOnInt {
		graceful = true
	} else if sig == syscall.SIGTERM && mconfig.LeaveOnTerm {
		graceful = true
	}

	// Bail fast if not doing a graceful leave
	if !graceful {
		return 1
	}

	// Attempt a graceful leave
	gracefulCh := make(chan struct{})
	c.Ui.Output("Gracefully shutting Maya server...")
	go func() {
		if err := c.maya.Leave(); err != nil {
			c.Ui.Error(fmt.Sprintf("Error: %s", err))
			return
		}
		close(gracefulCh)
	}()

	// Wait for leave or another signal
	select {
	case <-signalCh:
		return 1
	case <-time.After(gracefulTimeout):
		return 1
	case <-gracefulCh:
		return 0
	}
}

// handleReload is invoked when we should reload our configs, e.g. SIGHUP
func (c *UpCommand) handleReload(mconfig *server.MayaConfig) *server.MayaConfig {
	c.Ui.Output("Reloading Maya server configuration...")
	newConf := c.readMayaConfig()
	if newConf == nil {
		c.Ui.Error(fmt.Sprintf("Failed to reload config"))
		return mconfig
	}

	// Change the log level
	minLevel := logutils.LogLevel(strings.ToUpper(newConf.LogLevel))
	if server.ValidateLevelFilter(minLevel, c.logFilter) {
		c.logFilter.SetMinLevel(minLevel)
	} else {
		c.Ui.Error(fmt.Sprintf(
			"Invalid log level: %s. Valid log levels are: %v",
			minLevel, c.logFilter.Levels))

		// Keep the current log level
		newConf.LogLevel = mconfig.LogLevel
	}
	return newConf
}

func (c *UpCommand) Synopsis() string {
	return "Runs Maya server"
}

func (c *UpCommand) Help() string {
	helpText := `
Usage: mayaserver up [options]

  Starts Maya server and runs until an interrupt is received.
  The agent may be a client and/or server.

  The Maya server's configuration primarily comes from the config
  files used, but a subset of the options may also be passed directly
  as CLI arguments, listed below.

General Options :

  -bind=<addr>
    The address the agent will bind to for all of its various network
    services. The individual services that run bind to individual
    ports on this address. Defaults to the loopback 127.0.0.1.

  -config=<path>
    The path to either a single config file or a directory of config
    files to use for configuring Maya server. This option may be
    specified multiple times. If multiple config files are used, the
    values from each will be merged together. During merging, values
    from files found later in the list are merged over values from
    previously parsed files.

  -data-dir=<path>
    The data directory used to store state and other persistent data.
    On client machines this is used to house allocation data such as
    downloaded artifacts used by drivers. On server nodes, the data
    dir is also used to store the replicated log.

  -dc=<datacenter>
    The name of the datacenter this server is a member of. By
    default this is set to "dc1".

  -log-level=<level>
    Specify the verbosity level of Maya server's logs. Valid values include
    DEBUG, INFO, and WARN, in decreasing order of verbosity. The
    default is INFO.

  -node=<name>
    The name of the local agent. This name is used to identify the node
    in the cluster. The name must be unique per region. The default is
    the current hostname of the machine.

  -region=<region>
    Name of the region the Maya server will be a member of. By default
    this value is set to "global".
 `
	return strings.TrimSpace(helpText)
}

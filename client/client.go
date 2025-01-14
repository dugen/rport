package chclient

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/cloudradar-monitoring/rport/share/random"

	"github.com/denisbrodbeck/machineid"
	"github.com/pkg/errors"

	"github.com/gorilla/websocket"
	"github.com/jpillora/backoff"
	"github.com/shirou/gopsutil/v3/host"
	"golang.org/x/crypto/ssh"
	"golang.org/x/net/proxy"

	"github.com/cloudradar-monitoring/rport/client/monitoring"
	"github.com/cloudradar-monitoring/rport/client/system"
	"github.com/cloudradar-monitoring/rport/client/updates"
	chshare "github.com/cloudradar-monitoring/rport/share"
	"github.com/cloudradar-monitoring/rport/share/comm"
	"github.com/cloudradar-monitoring/rport/share/files"
	"github.com/cloudradar-monitoring/rport/share/logger"
	"github.com/cloudradar-monitoring/rport/share/models"
)

const ConnectionTimeout = 10 * time.Second

// Client represents a client instance
type Client struct {
	*logger.Logger

	SessionID          string
	configHolder       *ClientConfigHolder
	sshConfig          *ssh.ClientConfig
	sshConn            ssh.Conn
	running            bool
	runningc           chan error
	connStats          chshare.ConnStats
	cmdExec            system.CmdExecutor
	systemInfo         system.SysInfo
	updates            *updates.Updates
	monitor            *monitoring.Monitor
	serverCapabilities *models.Capabilities
	filesAPI           files.FileAPI
	watchdog           *Watchdog

	mu sync.RWMutex
}

// NewClient creates a new client instance
func NewClient(config *ClientConfigHolder, filesAPI files.FileAPI) (*Client, error) {
	// Generate a session id that will not change while the client is running
	// This allows the server to resume sessions.
	sessionID, err := random.UUID4()
	if err != nil {
		return nil, fmt.Errorf("failed to create initial session id: %s", err)
	}
	cmdExec := system.NewCmdExecutor(logger.NewLogger("cmd executor", config.Logging.LogOutput, config.Logging.LogLevel))
	logger := logger.NewLogger("client", config.Logging.LogOutput, config.Logging.LogLevel)
	watchdog, err := NewWatchdog(config.Connection.WatchdogIntegration, config.Client.DataDir, logger)
	if err != nil {
		return nil, fmt.Errorf("failed to create watchdog: %s", err)
	}
	logger.Infof("Client started with sessionID %s", sessionID)
	systemInfo := system.NewSystemInfo(cmdExec)
	client := &Client{
		SessionID:    sessionID,
		Logger:       logger,
		configHolder: config,
		running:      true,
		runningc:     make(chan error, 1),
		cmdExec:      cmdExec,
		systemInfo:   systemInfo,
		updates:      updates.New(logger, config.Client.UpdatesInterval),
		monitor:      monitoring.NewMonitor(logger, config.Monitoring, systemInfo),
		filesAPI:     filesAPI,
		watchdog:     watchdog,
	}

	client.sshConfig = &ssh.ClientConfig{
		User:            config.Client.AuthUser,
		Auth:            []ssh.AuthMethod{ssh.Password(config.Client.AuthPass)},
		ClientVersion:   "SSH-" + chshare.ProtocolVersion + "-client",
		HostKeyCallback: client.verifyServer,
		Timeout:         30 * time.Second,
	}

	return client, nil
}

// Run starts client and blocks while connected
func (c *Client) Run() error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := c.Start(ctx); err != nil {
		return err
	}
	return c.Wait()
}

func (c *Client) verifyServer(hostname string, remote net.Addr, key ssh.PublicKey) error {
	got := chshare.FingerprintKey(key)
	if c.configHolder.Client.Fingerprint != "" && !strings.HasPrefix(got, c.configHolder.Client.Fingerprint) {
		return fmt.Errorf("invalid fingerprint (%s)", got)
	}
	//overwrite with complete fingerprint
	c.Infof("Server's full fingerprint %s", got)
	return nil
}

// Start client and do not block
func (c *Client) Start(ctx context.Context) error {

	//optional keepalive loop
	if c.configHolder.Connection.KeepAlive > 0 {
		c.Infof("Keepalive job (client to server ping) started with interval %s", c.configHolder.Connection.KeepAlive)
		go c.keepAliveLoop()
	}
	//connection loop
	go c.connectionLoop(ctx)

	c.updates.Start(ctx)

	return nil
}

func (c *Client) keepAliveLoop() {
	for c.running {
		time.Sleep(c.configHolder.Connection.KeepAlive)

		c.mu.RLock()
		conn := c.sshConn
		c.mu.RUnlock()

		if conn != nil {
			ok, _, rtt, err := comm.PingConnectionWithTimeout(conn, c.configHolder.Connection.KeepAliveTimeout)
			if err != nil || !ok {
				c.Errorf("Failed to send keepalive (client to server ping): %s", err)
				c.sshConn.Close()
			} else {
				msg := fmt.Sprintf("ping to %s succeeded within %s", conn.RemoteAddr(), rtt)
				c.Debugf(msg)
				c.watchdog.Ping(WatchdogStateConnected, msg)
			}
		}
	}
}

func (c *Client) connectionLoop(ctx context.Context) {
	//connection loop!
	var connerr error
	switchbackChan := make(chan *sshClientConn, 1)
	b := &backoff.Backoff{Max: c.configHolder.Connection.MaxRetryInterval}
	for c.running {
		if connerr != nil {
			attempt := int(b.Attempt())
			var d = b.Duration()
			c.showConnectionError(connerr, attempt)
			if c.configHolder.Connection.MaxRetryCount >= 0 && attempt >= c.configHolder.Connection.MaxRetryCount {
				break // Stop trying to connect if the user has set a max retry limit
			}
			if _, ok := connerr.(comm.TimeoutError); ok {
				// Timeout means the server is available. No need to wait up to 5 min to try again.
				rand.Seed(time.Now().UnixNano())
				d = time.Duration(rand.Intn(20)) * time.Second
				b.Reset()
			}
			msg := fmt.Sprintf("Retrying in %s...", d)
			c.Infof(msg)
			c.watchdog.Ping(WatchdogStateReconnecting, msg)
			connerr = nil
			chshare.SleepSignal(d)
		}

		var sshConn *sshClientConn
		var isPrimary bool
		select {
		// When switchback to main server succeeds we get connection on the channel, otherwise try to connect
		case sshConn = <-switchbackChan:
			isPrimary = true
		default:
			var err error
			sshConn, isPrimary, err = c.connectToMainOrFallback()
			if err != nil {
				connerr = err // Setting a connerr causes the loop to sleep and try again later
				continue
			}
		}

		// Start handling requests and channels immediately, otherwise ssh connection might hang
		go c.handleSSHRequests(ctx, sshConn)
		go c.connectStreams(sshConn.Channels)

		switchbackCtx, cancelSwitchback := context.WithCancel(ctx)
		if !isPrimary {
			go func() {
				for {
					switchbackTimer := time.NewTimer(c.configHolder.Client.ServerSwitchbackInterval)
					select {
					case <-switchbackCtx.Done():
						switchbackTimer.Stop()
						return
					case <-switchbackTimer.C:
						switchbackConn, err := c.connect(c.configHolder.Client.Server)
						if err != nil {
							c.Errorf("Switchback failed: %v", err.Error())
							continue
						}
						c.Infof("Connected to main server, switching back.")
						switchbackChan <- switchbackConn
						sshConn.Connection.Close()
						return
					}
				}
			}()
		}

		err := c.sendConnectionRequest(ctx, sshConn.Connection)
		if err != nil {
			// Connection request has failed, we try again
			cancelSwitchback()
			connerr = err
			continue
		}
		// Connection request has succeeded
		b.Reset()

		c.mu.Lock()
		c.sshConn = sshConn.Connection // Hand over the open SSH connection to the client
		c.mu.Unlock()

		c.updates.SetConn(sshConn.Connection)
		c.monitor.SetConn(sshConn.Connection)

		err = sshConn.Connection.Wait() // Block aka wait until the connection is closed

		c.mu.Lock()
		//disconnected
		c.sshConn = nil
		c.mu.Unlock()

		c.updates.SetConn(nil)
		c.monitor.SetConn(nil)
		c.monitor.Stop()
		cancelSwitchback()

		// use of closed network connection happens when switchback closes the connection, ignore the error
		if err != nil && err != io.EOF && !strings.HasSuffix(err.Error(), "use of closed network connection") {
			connerr = err
		}

		c.Infof("Disconnected\n")
	}
	close(c.runningc)
}

type sshClientConn struct {
	Connection ssh.Conn
	Channels   <-chan ssh.NewChannel
	Requests   <-chan *ssh.Request
}

func (c *Client) connectToMainOrFallback() (conn *sshClientConn, isPrimary bool, err error) {
	servers := append([]string{c.configHolder.Client.Server}, c.configHolder.Client.FallbackServers...)
	for i, server := range servers {
		conn, err = c.connect(server)
		if err != nil {
			continue // Try the next server in the list
		}
		return conn, i == 0, nil
	}
	return nil, false, err
}

func (c *Client) connect(server string) (*sshClientConn, error) {
	via := ""
	if c.configHolder.Client.ProxyURL != nil {
		via = " via " + c.configHolder.Client.ProxyURL.String()
	}
	c.Infof("Trying to connect to %s%s ...\n", server, via)

	netDialer := &net.Dialer{}
	d := websocket.Dialer{
		ReadBufferSize:   1024,
		WriteBufferSize:  1024,
		HandshakeTimeout: 45 * time.Second,
		Subprotocols:     []string{chshare.ProtocolVersion},
		NetDialContext:   netDialer.DialContext,
	}
	if c.configHolder.Client.BindInterface != "" {
		laddr, err := c.localAddrForInterface(c.configHolder.Client.BindInterface)
		if err != nil {
			return nil, err
		}
		netDialer.LocalAddr = laddr
	}
	//optionally proxy
	if c.configHolder.Client.ProxyURL != nil {
		if strings.HasPrefix(c.configHolder.Client.ProxyURL.Scheme, "socks") {
			// SOCKS5 proxy
			if c.configHolder.Client.ProxyURL.Scheme != "socks" && c.configHolder.Client.ProxyURL.Scheme != "socks5h" {
				return nil, fmt.Errorf(
					"unsupported socks proxy type: %s:// (only socks5h:// or socks:// is supported)",
					c.configHolder.Client.ProxyURL.Scheme)
			}
			var auth *proxy.Auth
			if c.configHolder.Client.ProxyURL.User != nil {
				pass, _ := c.configHolder.Client.ProxyURL.User.Password()
				auth = &proxy.Auth{
					User:     c.configHolder.Client.ProxyURL.User.Username(),
					Password: pass,
				}
			}
			socksDialer, err := proxy.SOCKS5("tcp", c.configHolder.Client.ProxyURL.Host, auth, netDialer)
			if err != nil {
				return nil, err
			}
			d.NetDialContext = socksDialer.(proxy.ContextDialer).DialContext
		} else {
			// CONNECT proxy
			d.Proxy = func(*http.Request) (*url.URL, error) {
				return c.configHolder.Client.ProxyURL, nil
			}
		}
	}
	wsConn, _, err := d.Dial(server, c.configHolder.Connection.HTTPHeaders)
	if err != nil {
		return nil, ConnectionErrorHints(server, c.Logger, err)
	}
	conn := chshare.NewWebSocketConn(wsConn)
	// perform SSH handshake on net.Conn
	c.Debugf("Handshaking...")
	sshConn, chans, reqs, err := ssh.NewClientConn(conn, "", c.sshConfig)
	if err != nil {
		if strings.Contains(err.Error(), "unable to authenticate") {
			c.Errorf("Authentication failed")
			return nil, err
		}
		return nil, err
	}
	return &sshClientConn{
		Connection: sshConn,
		Requests:   reqs,
		Channels:   chans,
	}, nil
}

func (c *Client) sendConnectionRequest(ctx context.Context, sshConn ssh.Conn) error {
	connReq, err := c.connectionRequest(ctx)
	if err != nil {
		return err
	}

	req, err := chshare.EncodeConnectionRequest(connReq)
	if err != nil {
		return fmt.Errorf("could not encode connection request: %v", err)
	}
	c.Infof("Sending connection request.")
	c.Debugf("Sending connection request with client details %s", string(req))
	t0 := time.Now()
	replyOk, respBytes, err := comm.SendRequestWithTimeout(sshConn, "new_connection", true, req, ConnectionTimeout)
	if err != nil {
		if err2 := sshConn.Close(); err2 != nil {
			c.Errorf("Failed to close connection: %s", err2)
		}
		return err
	}
	c.Debugf("Connection request has been answered successfully within %s.", time.Since(t0))
	if !replyOk {
		msg := string(respBytes)

		// if replied with client credentials already used - retry
		if strings.Contains(msg, "client is already connected:") {
			if closeErr := sshConn.Close(); closeErr != nil {
				c.Errorf(closeErr.Error())
			}
			return errors.New("client is already connected or previous session was not properly closed")
		}

		return errors.New(msg)
	}
	var remotes []*models.Remote
	err = json.Unmarshal(respBytes, &remotes)
	if err != nil {
		return fmt.Errorf("can't decode reply payload: %s", err)
	}
	msg := fmt.Sprintf("Connected to %s within %s", sshConn.RemoteAddr().String(), time.Since(t0))
	c.watchdog.Ping(WatchdogStateConnected, msg)
	c.Infof(msg)
	for _, r := range remotes {
		c.Infof("New tunnel: %s", r.String())

		serverStr := r.Local()
		if r.HTTPProxy {
			serverStr = "https://" + serverStr
		}

		c.Infof("Local port %s has become available on %s", r.Remote(), serverStr)
	}

	return nil
}

// afterPutCapabilities is the place to do things dependent on server capabilities
func (c *Client) afterPutCapabilities(ctx context.Context) {
	if c.serverCapabilities.MonitoringVersion > 0 {
		c.monitor.Start(ctx)
	} else {
		c.Debugf("Server has no monitoring capability, measurement not started")
	}
}

func (c *Client) handlePutCapabilitiesRequest(ctx context.Context, payload []byte) {
	caps := &models.Capabilities{}
	if err := json.Unmarshal(payload, caps); err != nil {
		c.Errorf("failed to decode %T: %v", caps, err)
		return
	}
	c.Debugf("Server has capabilities: %s", string(payload))
	c.serverCapabilities = caps
	c.afterPutCapabilities(ctx)
}

func (c *Client) handleSSHRequests(ctx context.Context, sshConn *sshClientConn) {
	for r := range sshConn.Requests {
		var err error
		var resp interface{}
		switch r.Type {
		case comm.RequestTypeCheckPort:
			resp, err = checkPort(r.Payload)
		case comm.RequestTypeRunCmd:
			resp, err = c.HandleRunCmdRequest(ctx, r.Payload)
		case comm.RequestTypeRefreshUpdatesStatus:
			c.updates.Refresh()
		case comm.RequestTypePutCapabilities:
			c.handlePutCapabilitiesRequest(ctx, r.Payload)
		case comm.RequestTypeUpload:
			uploadManager := NewSSHUploadManager(
				c.Logger,
				c.filesAPI,
				c.configHolder,
				sshConn.Connection,
				system.SysUserProvider{},
			)

			resp, err = uploadManager.HandleUploadRequest(r.Payload)
		case comm.RequestTypeCheckTunnelAllowed:
			resp, err = c.checkTunnelAllowed(r.Payload)
		case comm.RequestTypePing:
			_ = r.Reply(true, nil)
		default:
			c.Debugf("Unknown request: %q", r.Type)
			comm.ReplyError(c.Logger, r, errors.New("unknown request"))
			continue
		}

		if err != nil {
			c.Errorf("Failed to handle %q request: %v", r.Type, err)
			comm.ReplyError(c.Logger, r, err)
			continue
		}

		comm.ReplySuccessJSON(c.Logger, r, resp)
	}
}

func checkPort(payload []byte) (*comm.CheckPortResponse, error) {
	req, err := comm.DecodeCheckPortRequest(payload)
	if err != nil {
		return nil, err
	}

	open, checkErr := system.IsPortOpen(req.HostPort, req.Timeout)
	var errMsg string
	if checkErr != nil {
		errMsg = checkErr.Error()
	}
	return &comm.CheckPortResponse{
		Open:   open,
		ErrMsg: errMsg,
	}, nil
}

func (c *Client) checkTunnelAllowed(payload []byte) (*comm.CheckTunnelAllowedResponse, error) {
	var req comm.CheckTunnelAllowedRequest
	err := json.Unmarshal(payload, &req)
	if err != nil {
		return nil, err
	}

	allowed, err := TunnelIsAllowed(c.configHolder.Client.TunnelAllowed, req.Remote)
	if err != nil {
		return nil, err
	}

	return &comm.CheckTunnelAllowedResponse{
		IsAllowed: allowed,
	}, nil
}

func (c *Client) showConnectionError(connerr error, attempt int) {
	maxAttempt := c.configHolder.Connection.MaxRetryCount
	//show error and attempt counts
	msg := fmt.Sprintf("Connection error: %s", connerr)
	if attempt > 0 {
		maxAttemptStr := fmt.Sprint(maxAttempt)
		if maxAttempt < 0 {
			maxAttemptStr = "infinite"
		}
		msg += fmt.Sprintf(" (Attempt: %d of %s)", attempt, maxAttemptStr)
	}
	c.Errorf(msg)
	if strings.Contains(msg, "previous session was not properly closed") {
		c.Infof("Server will clean up orphaned sessions within its {check_clients_connection_interval} automatically.")
	}
}

// Wait blocks while the client is running.
// Can only be called once.
func (c *Client) Wait() error {
	return <-c.runningc
}

// Close manually stops the client
func (c *Client) Close() error {
	c.running = false
	c.watchdog.Close()
	if c.sshConn == nil {
		return nil
	}
	return c.sshConn.Close()
}

func (c *Client) connectStreams(chans <-chan ssh.NewChannel) {
	for ch := range chans {
		remote := string(ch.ExtraData())
		protocol := models.ProtocolTCP
		parts := strings.SplitN(remote, "/", 2)
		if len(parts) == 2 {
			remote = parts[0]
			protocol = parts[1]
		}

		allowed, err := TunnelIsAllowed(c.configHolder.Client.TunnelAllowed, remote)
		if err != nil {
			c.Errorf("Could not check if remote is allowed: %v", err)
		}
		if !allowed {
			c.Infof(`Rejecting stream to %s based on "tunnel_allowed" config.`, remote)
			err := ch.Reject(ssh.Prohibited, `not allowed with "tunnel_allowed" config`)
			if err != nil {
				c.Errorf("Failed to reject stream: %v", err)
			}
			continue
		}

		stream, reqs, err := ch.Accept()
		if err != nil {
			c.Debugf("Failed to accept stream: %s", err)
			continue
		}
		go ssh.DiscardRequests(reqs)

		switch protocol {
		case models.ProtocolTCP:
			l := c.Logger.Fork("tcp conn#%d", c.connStats.New())
			go chshare.HandleTCPStream(l, &c.connStats, stream, remote)
		case models.ProtocolUDP:
			go func() {
				err := newUDPHandler(c.Logger.Fork("udp#%s", remote), remote).Handle(stream)
				if err != nil {
					c.Errorf("Error with UDP: %v", err)
				}
			}()
		default:
			c.Errorf("Unsupported protocol %v for tunnel %v", protocol, remote)
			stream.Close()
		}
	}
}

// returns all local ipv4, ipv6 addresses
func (c *Client) localIPAddresses() ([]string, []string, error) {
	ipv4 := []string{}
	ipv6 := []string{}

	addrs, err := c.systemInfo.InterfaceAddrs()
	if err != nil {
		return nil, nil, err
	}
	for _, addr := range addrs {
		var ip net.IP
		switch v := addr.(type) {
		case *net.IPNet:
			ip = v.IP
		case *net.IPAddr:
			ip = v.IP
		}
		if ip.IsLoopback() {
			continue
		}
		if ip.To4() != nil {
			ipv4 = append(ipv4, ip.String())
		} else if ip.To16() != nil {
			ipv6 = append(ipv6, ip.String())
		}
	}
	return ipv4, ipv6, nil
}

func (c *Client) connectionRequest(ctx context.Context) (*chshare.ConnectionRequest, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Second*5)
	defer cancel()

	connReq := &chshare.ConnectionRequest{
		ID:                     c.configHolder.Client.ID,
		Name:                   c.configHolder.Client.Name,
		SessionID:              c.SessionID,
		Tags:                   c.configHolder.Client.Tags,
		Remotes:                c.configHolder.Client.Tunnels,
		OS:                     system.UnknownValue,
		OSArch:                 c.systemInfo.GoArch(),
		OSKernel:               system.UnknownValue,
		OSFamily:               system.UnknownValue,
		OSVersion:              system.UnknownValue,
		OSVirtualizationRole:   system.UnknownValue,
		OSVirtualizationSystem: system.UnknownValue,
		Version:                chshare.BuildVersion,
		Hostname:               system.UnknownValue,
		CPUFamily:              system.UnknownValue,
		CPUModel:               system.UnknownValue,
		CPUModelName:           system.UnknownValue,
		CPUVendor:              system.UnknownValue,
		ClientConfiguration:    c.configHolder.Config,
	}

	var err error
	if connReq.ID == "" && c.configHolder.Client.UseSystemID {
		connReq.ID, err = machineid.ID()
		if err != nil {
			return nil, fmt.Errorf("could not use system id as client id: try to set client.id manually or disable client.use_system_id. Error: %w", err)
		}
	}

	if connReq.Name == "" && c.configHolder.Client.UseHostname {
		connReq.Name, err = c.systemInfo.Hostname()
		if err != nil {
			return nil, fmt.Errorf("could not use system hostname as client name: try to set client.name manually or disable client.use_hostname. Error: %w", err)
		}
	}

	info, err := c.systemInfo.HostInfo(ctx)
	if err != nil {
		c.Logger.Errorf("Could not get os information: %v", err)
	} else {
		connReq.OSKernel = info.OS
		connReq.OSFamily = info.PlatformFamily
	}

	os, err := c.getOS(ctx, info)
	if err != nil {
		c.Logger.Errorf("Could not get os name: %v", err)
	} else {
		connReq.OS = os
	}

	connReq.OSFullName = c.getOSFullName(info)
	if info != nil && info.PlatformVersion != "" {
		connReq.OSVersion = info.PlatformVersion
	}

	oSVirtualizationSystem, oSVirtualizationRole, err := c.systemInfo.VirtualizationInfo(ctx)
	if err != nil {
		c.Logger.Errorf("Could not get OS Virtualization Info: %v", err)
	} else {
		connReq.OSVirtualizationSystem = oSVirtualizationSystem
		connReq.OSVirtualizationRole = oSVirtualizationRole
	}

	connReq.IPv4, connReq.IPv6, err = c.localIPAddresses()
	if err != nil {
		c.Logger.Errorf("Could not get local ips: %v", err)
	}

	hostname, err := c.systemInfo.Hostname()
	if err != nil {
		c.Logger.Errorf("Could not get hostname: %v", err)
	} else {
		connReq.Hostname = hostname
	}

	cpuInfo, err := c.systemInfo.CPUInfo(ctx)

	if err != nil {
		c.Logger.Errorf("Could not get cpu information: %v", err)
	}

	if len(cpuInfo.CPUs) > 0 {
		connReq.CPUFamily = cpuInfo.CPUs[0].Family
		connReq.CPUModel = cpuInfo.CPUs[0].Model
		connReq.CPUModelName = cpuInfo.CPUs[0].ModelName
		connReq.CPUVendor = cpuInfo.CPUs[0].VendorID
	}
	connReq.NumCPUs = cpuInfo.NumCores

	memoryInfo, err := c.systemInfo.MemoryStats(ctx)
	if err != nil {
		c.Logger.Errorf("Could not get memory information: %v", err)
	} else if memoryInfo != nil {
		connReq.MemoryTotal = memoryInfo.Total
	}

	connReq.Timezone = c.getTimezone()

	return connReq, nil
}

func (c *Client) getOS(ctx context.Context, info *host.InfoStat) (string, error) {
	if info == nil {
		return system.UnknownValue, nil
	} else if info.OS == "windows" {
		return info.Platform + " " + info.PlatformVersion + " " + info.PlatformFamily, nil
	}
	return c.systemInfo.Uname(ctx)
}

func (c *Client) getOSFullName(infoStat *host.InfoStat) string {
	if infoStat == nil {
		return system.UnknownValue
	}

	return fmt.Sprintf("%s %s", strings.Title(strings.ToLower(infoStat.Platform)), infoStat.PlatformVersion)
}

func (c *Client) getTimezone() string {
	return c.systemInfo.SystemTime().Format("MST (UTC-07:00)")
}

func (c *Client) localAddrForInterface(ifaceName string) (net.Addr, error) {
	iface, err := net.InterfaceByName(ifaceName)
	if err != nil {
		return nil, errors.Wrapf(err, "unable to find %s", ifaceName)
	}
	addrs, err := iface.Addrs()
	if err != nil {
		return nil, errors.Wrapf(err, "unable to get address for %s", ifaceName)
	}
	var selected net.IP
	for _, a := range addrs {
		ip, _, err := net.ParseCIDR(a.String())
		if err != nil {
			return nil, err
		}
		if ip.IsUnspecified() {
			continue
		}
		selected = ip
		break
	}
	if selected == nil {
		return nil, errors.Errorf("no address found for %s", ifaceName)
	}
	laddr, err := net.ResolveTCPAddr("tcp", fmt.Sprintf("%v:0", selected))
	if err != nil {
		return nil, errors.Wrapf(err, "could not resolve tcp address for %s", ifaceName)
	}

	c.Infof("Connecting using %s (%s)", iface.Name, selected)

	return laddr, nil
}

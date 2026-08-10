// pkg/opamp/client.go
// OpAMP client wrapper using the official opamp-go library with protobuf.

package opamp

import (
	"context"
	"crypto/md5"
	"crypto/tls"
	"fmt"
	"net/http"
	"strings"
	"sync"

	opampclient "github.com/open-telemetry/opamp-go/client"
	opampcltypes "github.com/open-telemetry/opamp-go/client/types"
	"github.com/open-telemetry/opamp-go/protobufs"

	"github.com/collectorctrl/collectorctrl/pkg/api"
)

// ClientConfig configures the fleet-level OpAMP client.
type ClientConfig struct {
	Endpoint   string
	Headers    map[string]string
	TLSConfig  *tls.Config
	AgentID    string
	AgentType  api.AgentType
	Labels     map[string]string
	K8sContext *api.K8sContext
}

// Client wraps the official opamp-go client.
type Client struct {
	config ClientConfig

	client    opampclient.OpAMPClient
	connected bool
	connMu    sync.RWMutex

	effectiveConfig string
	configMu        sync.RWMutex

	onConfigUpdate func(ConfigUpdate)
	onEmergencyCmd func(EmergencyCommand)
	onFetchLogs    func(string)
}

// ConfigUpdate is sent by the server to push a new configuration.
type ConfigUpdate struct {
	ConfigYAML      string
	ConfigHash      string
	RolloutStrategy *api.RolloutStrategy
}

// EmergencyCommand is sent by the server to force an immediate config change.
type EmergencyCommand struct {
	ConfigYAML string
	Reason     string
}

// NewClient creates a fleet-level OpAMP client.
func NewClient(cfg ClientConfig) *Client {
	return &Client{config: cfg}
}

// OnConfigUpdate registers a handler for normal config pushes.
func (c *Client) OnConfigUpdate(fn func(ConfigUpdate)) {
	c.onConfigUpdate = fn
}

// OnEmergencyCmd registers a handler for emergency override commands.
func (c *Client) OnEmergencyCmd(fn func(EmergencyCommand)) {
	c.onEmergencyCmd = fn
}

// OnFetchLogs registers a handler for retrieving collector pod logs.
func (c *Client) OnFetchLogs(fn func(string)) {
	c.onFetchLogs = fn
}

// Start establishes the OpAMP connection.
func (c *Client) Start(ctx context.Context) error {
	c.client = opampclient.NewWebSocket(nil)

	headers := make(http.Header)
	for k, v := range c.config.Headers {
		headers.Set(k, v)
	}

	capabilities := protobufs.AgentCapabilities_AgentCapabilities_ReportsHealth |
		protobufs.AgentCapabilities_AgentCapabilities_ReportsEffectiveConfig |
		protobufs.AgentCapabilities_AgentCapabilities_ReportsStatus |
		protobufs.AgentCapabilities_AgentCapabilities_AcceptsRestartCommand |
		protobufs.AgentCapabilities_AgentCapabilities_AcceptsRemoteConfig

	settings := opampcltypes.StartSettings{
		OpAMPServerURL: c.config.Endpoint,
		TLSConfig:      c.config.TLSConfig,
		InstanceUid:    opampcltypes.InstanceUid(c.instanceUID()),
		Header:         headers,
		Callbacks: opampcltypes.Callbacks{
			OnConnect: func(ctx context.Context) {
				c.connMu.Lock()
				c.connected = true
				c.connMu.Unlock()
			},
			OnConnectFailed: func(ctx context.Context, err error) { _ = err },
			OnError:         func(ctx context.Context, err *protobufs.ServerErrorResponse) { _ = err },
			GetEffectiveConfig: func(ctx context.Context) (*protobufs.EffectiveConfig, error) {
				return c.getEffectiveConfig(), nil
			},
			OnMessage: c.onMessage,
		},
		Capabilities: capabilities,
	}

	// Set agent description
	if err := c.client.SetAgentDescription(c.agentDescription()); err != nil {
		return fmt.Errorf("set agent description: %w", err)
	}

	// Set initial health BEFORE Start() — opamp-go requires this
	if err := c.client.SetHealth(&protobufs.ComponentHealth{
		Healthy: false,
		ComponentHealthMap: map[string]*protobufs.ComponentHealth{
			"fleet": {Healthy: false, Status: "initializing"},
		},
	}); err != nil {
		return fmt.Errorf("set initial health: %w", err)
	}

	if err := c.client.Start(ctx, settings); err != nil {
		return fmt.Errorf("opamp start: %w", err)
	}

	return nil
}

// Stop closes the OpAMP connection.
func (c *Client) Stop() {
	if c.client != nil {
		_ = c.client.Stop(context.Background())
	}
}

// PodHealth holds per-pod status for topology reporting.
type PodHealth struct {
	Name  string
	Node  string
	IP    string
	Ready bool
	Phase string
}

// SetHealth updates the health status with per-pod topology.
func (c *Client) SetHealth(health api.HealthStatus, pods []PodHealth) error {
	if c.client == nil {
		return fmt.Errorf("client not started")
	}

	ready := 0
	for _, p := range pods {
		if p.Ready {
			ready++
		}
	}
	total := len(pods)
	healthy := health == api.HealthHealthy

	componentHealthMap := map[string]*protobufs.ComponentHealth{
		"fleet": {
			Healthy: healthy,
			ComponentHealthMap: map[string]*protobufs.ComponentHealth{
				"total_pods": {Healthy: true, Status: fmt.Sprintf("%d", total)},
				"ready_pods": {Healthy: ready == total, Status: fmt.Sprintf("%d", ready)},
			},
		},
	}

	for _, p := range pods {
		status := fmt.Sprintf("node=%s ip=%s phase=%s", p.Node, p.IP, p.Phase)
		componentHealthMap["pod/"+p.Name] = &protobufs.ComponentHealth{
			Healthy: p.Ready,
			Status:  status,
		}
	}

	return c.client.SetHealth(&protobufs.ComponentHealth{
		Healthy:            healthy,
		ComponentHealthMap: componentHealthMap,
	})
}

// SetEffectiveConfig stores the config and triggers sending it to the server.
func (c *Client) SetEffectiveConfig(configYAML string) error {
	if c.client == nil {
		return fmt.Errorf("client not started")
	}
	c.configMu.Lock()
	c.effectiveConfig = configYAML
	c.configMu.Unlock()
	return c.client.UpdateEffectiveConfig(context.Background())
}

// SendEmergencyAck sends an emergency acknowledgement to the server.
func (c *Client) SendEmergencyAck(success bool, errMsg string) error {
	if c.client == nil {
		return fmt.Errorf("client not started")
	}
	data := fmt.Sprintf("emergency_ack:%v:%s", success, errMsg)
	_, err := c.client.SendCustomMessage(&protobufs.CustomMessage{
		Capability: "collectorctrl",
		Type:       "emergency_ack",
		Data:       []byte(data),
	})
	return err
}

// SendPodLogs sends the fetched pod logs back to the server.
func (c *Client) SendPodLogs(podName string, logs string, success bool, errMsg string) error {
	if c.client == nil {
		return fmt.Errorf("client not started")
	}
	var data string
	if success {
		data = fmt.Sprintf("fetch_logs_ack:%s:true:%s", podName, logs)
	} else {
		data = fmt.Sprintf("fetch_logs_ack:%s:false:%s", podName, errMsg)
	}
	_, err := c.client.SendCustomMessage(&protobufs.CustomMessage{
		Capability: "collectorctrl",
		Type:       "fetch_logs_response",
		Data:       []byte(data),
	})
	return err
}

// Connected returns true if the client is connected.
func (c *Client) Connected() bool {
	c.connMu.RLock()
	defer c.connMu.RUnlock()
	return c.connected
}

// instanceUID generates a stable UUID from the agent ID using MD5 hash.
func (c *Client) instanceUID() [16]byte {
	var uid [16]byte
	h := md5.Sum([]byte(c.config.AgentID))
	copy(uid[:], h[:])
	return uid
}

func (c *Client) agentDescription() *protobufs.AgentDescription {
	identifying := []*protobufs.KeyValue{
		keyVal("service.name", string(c.config.AgentType)),
		keyVal("service.instance.id", c.config.AgentID),
	}

	nonIdentifying := []*protobufs.KeyValue{}
	for k, v := range c.config.Labels {
		nonIdentifying = append(nonIdentifying, keyVal(k, v))
	}

	if kc := c.config.K8sContext; kc != nil {
		nonIdentifying = append(nonIdentifying,
			keyVal("k8s.cluster.name", kc.ClusterName),
			keyVal("k8s.namespace", kc.Namespace),
			keyVal("k8s.workload.type", kc.WorkloadType),
			keyVal("k8s.workload.name", kc.WorkloadName),
			keyVal("k8s.configmap.name", kc.ConfigMapName),
			keyVal("k8s.configmap.key", kc.ConfigMapKey),
		)
	}

	return &protobufs.AgentDescription{
		IdentifyingAttributes:    identifying,
		NonIdentifyingAttributes: nonIdentifying,
	}
}

func (c *Client) getEffectiveConfig() *protobufs.EffectiveConfig {
	c.configMu.RLock()
	cfg := c.effectiveConfig
	c.configMu.RUnlock()

	return &protobufs.EffectiveConfig{
		ConfigMap: &protobufs.AgentConfigMap{
			ConfigMap: map[string]*protobufs.AgentConfigFile{
				"": {Body: []byte(cfg), ContentType: "text/yaml"},
			},
		},
	}
}

func (c *Client) onMessage(ctx context.Context, msg *opampcltypes.MessageData) {
	if msg.RemoteConfig != nil && c.onConfigUpdate != nil {
		cfg := msg.RemoteConfig.Config
		if cfg != nil {
			for _, file := range cfg.GetConfigMap() {
				c.onConfigUpdate(ConfigUpdate{
					ConfigYAML: string(file.Body),
					ConfigHash: fmt.Sprintf("%x", msg.RemoteConfig.ConfigHash),
				})
				break
			}
		}
	}

	if msg.CustomMessage != nil {
		cm := msg.CustomMessage
		if strings.EqualFold(cm.Capability, "collectorctrl") {
			if strings.EqualFold(cm.Type, "emergency") && c.onEmergencyCmd != nil {
				data := string(cm.Data)
				parts := strings.SplitN(data, "\n---\n", 2)
				reason := "emergency override"
				configYAML := data
				if len(parts) == 2 {
					reason = strings.TrimSpace(strings.TrimPrefix(parts[0], "emergency:"))
					configYAML = parts[1]
				}
				c.onEmergencyCmd(EmergencyCommand{
					ConfigYAML: configYAML,
					Reason:     reason,
				})
			} else if strings.EqualFold(cm.Type, "fetch_logs") && c.onFetchLogs != nil {
				podName := string(cm.Data)
				c.onFetchLogs(podName)
			}
		}
	}
}

func keyVal(key, value string) *protobufs.KeyValue {
	return &protobufs.KeyValue{
		Key: key,
		Value: &protobufs.AnyValue{
			Value: &protobufs.AnyValue_StringValue{StringValue: value},
		},
	}
}

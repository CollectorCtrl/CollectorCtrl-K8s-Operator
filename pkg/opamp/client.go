package opamp

import (
	"context"
	"crypto/md5"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/collectorctrl/collectorctrl/pkg/api"
	opampclient "github.com/open-telemetry/opamp-go/client"
	opampcltypes "github.com/open-telemetry/opamp-go/client/types"
	"github.com/open-telemetry/opamp-go/protobufs"
)

type ClientConfig struct {
	Endpoint  string
	Headers   map[string]string
	TLSConfig *tls.Config
	AgentID   string
	AgentType api.AgentType
	Labels    map[string]string
}

// Observation is a Kubernetes API snapshot, never a runtime effective config.
// Keep this versioned wire contract in sync with the server's K8sObservation.
type Observation struct {
	Source             string      `json:"source"`
	RuntimeVerified    bool        `json:"runtime_verified"`
	ObservedAt         time.Time   `json:"observed_at"`
	Namespace          string      `json:"namespace"`
	WorkloadKind       string      `json:"workload_kind"`
	WorkloadName       string      `json:"workload_name"`
	WorkloadUID        string      `json:"workload_uid"`
	CollectorContainer string      `json:"collector_container"`
	ConfigMapName      string      `json:"configmap_name"`
	ConfigMapKey       string      `json:"configmap_key"`
	ResourceVersion    string      `json:"resource_version"`
	ConfigHash         string      `json:"config_hash"`
	ConfigYAML         string      `json:"config_yaml,omitempty"`
	DesiredPods        int         `json:"desired_pods"`
	ReadyPods          int         `json:"ready_pods"`
	Pods               []PodHealth `json:"pods"`
}
type PodHealth struct {
	Name     string `json:"name"`
	Node     string `json:"node"`
	Ready    bool   `json:"ready"`
	Phase    string `json:"phase"`
	Image    string `json:"image"`
	Restarts int32  `json:"restarts"`
	Reason   string `json:"reason,omitempty"`
}

type Client struct {
	config    ClientConfig
	client    opampclient.OpAMPClient
	mu        sync.RWMutex
	connected bool
}

func NewClient(cfg ClientConfig) *Client { return &Client{config: cfg} }

// TLSConfig trusts system CAs and optionally an additional PEM CA bundle.
// There is intentionally no skip-verification option.
func TLSConfig(caFile string) (*tls.Config, error) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if caFile == "" {
		return cfg, nil
	}
	pem, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("read OpAMP CA bundle: %w", err)
	}
	roots, err := x509.SystemCertPool()
	if err != nil {
		roots = x509.NewCertPool()
	}
	if !roots.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("OpAMP CA bundle contains no valid certificates")
	}
	cfg.RootCAs = roots
	return cfg, nil
}

func (c *Client) Start(ctx context.Context) error {
	c.client = opampclient.NewWebSocket(nil)
	if err := c.client.SetCustomCapabilities(&protobufs.CustomCapabilities{Capabilities: []string{"collectorctrl"}}); err != nil {
		return err
	}
	headers := make(http.Header)
	for k, v := range c.config.Headers {
		headers.Set(k, v)
	}
	attrs := []*protobufs.KeyValue{}
	for k, v := range c.config.Labels {
		attrs = append(attrs, keyVal(k, v))
	}
	if err := c.client.SetAgentDescription(&protobufs.AgentDescription{
		IdentifyingAttributes:    []*protobufs.KeyValue{keyVal("service.name", string(c.config.AgentType)), keyVal("service.instance.id", c.config.AgentID)},
		NonIdentifyingAttributes: attrs,
	}); err != nil {
		return err
	}
	if err := c.client.SetHealth(&protobufs.ComponentHealth{Healthy: false, Status: "Collector readiness unknown; telemetry delivery unverified"}); err != nil {
		return err
	}
	return c.client.Start(ctx, opampcltypes.StartSettings{
		OpAMPServerURL: c.config.Endpoint, TLSConfig: c.config.TLSConfig,
		InstanceUid: opampcltypes.InstanceUid(md5.Sum([]byte(c.config.AgentID))), Header: headers,
		Capabilities: protobufs.AgentCapabilities_AgentCapabilities_ReportsHealth | protobufs.AgentCapabilities_AgentCapabilities_ReportsStatus,
		Callbacks: opampcltypes.Callbacks{
			OnConnect:       func(context.Context) { c.setConnected(true) },
			OnConnectFailed: func(context.Context, error) { c.setConnected(false) },
			OnError:         func(context.Context, *protobufs.ServerErrorResponse) { c.setConnected(false) },
			OnCommand: func(context.Context, *protobufs.ServerToAgentCommand) error {
				return fmt.Errorf("observation mode does not accept commands")
			},
			OnMessage: func(_ context.Context, msg *opampcltypes.MessageData) {
				if msg.CustomMessage != nil && msg.CustomMessage.Capability == "collectorctrl" && msg.CustomMessage.Type == "emergency" {
					_, _ = c.client.SendCustomMessage(&protobufs.CustomMessage{Capability: "collectorctrl", Type: "emergency_ack", Data: []byte("emergency_ack:false:operator is observation-only")})
				}
			},
		},
	})
}
func (c *Client) setConnected(v bool) { c.mu.Lock(); defer c.mu.Unlock(); c.connected = v }
func (c *Client) Connected() bool     { c.mu.RLock(); defer c.mu.RUnlock(); return c.connected }
func (c *Client) Stop() {
	c.setConnected(false)
	if c.client != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = c.client.Stop(ctx)
	}
}

func readinessHealth(o Observation) *protobufs.ComponentHealth {
	healthy := o.DesiredPods > 0 && o.ReadyPods >= o.DesiredPods
	components := map[string]*protobufs.ComponentHealth{
		"fleet/total_pods":   {Healthy: true, Status: fmt.Sprint(len(o.Pods))},
		"fleet/ready_pods":   {Healthy: healthy, Status: fmt.Sprint(o.ReadyPods)},
		"fleet/desired_pods": {Healthy: true, Status: fmt.Sprint(o.DesiredPods)},
	}
	for _, p := range o.Pods {
		components["pod/"+p.Name] = &protobufs.ComponentHealth{Healthy: p.Ready, Status: fmt.Sprintf("node=%s phase=%s restarts=%d reason=%s", p.Node, p.Phase, p.Restarts, p.Reason)}
	}
	return &protobufs.ComponentHealth{Healthy: healthy, Status: "Collector container readiness only; telemetry delivery unverified", ComponentHealthMap: components}
}
func (c *Client) Report(o Observation) error {
	if c.client == nil {
		return fmt.Errorf("client not started")
	}
	if err := c.client.SetHealth(readinessHealth(o)); err != nil {
		return err
	}
	if !c.Connected() {
		return nil
	}
	payload, err := json.Marshal(o)
	if err != nil {
		return err
	}
	_, err = c.client.SendCustomMessage(&protobufs.CustomMessage{Capability: "collectorctrl", Type: "k8s_observation_v1", Data: payload})
	if errors.Is(err, opampcltypes.ErrCustomMessagePending) {
		return nil
	}
	return err
}
func keyVal(k, v string) *protobufs.KeyValue {
	return &protobufs.KeyValue{Key: k, Value: &protobufs.AnyValue{Value: &protobufs.AnyValue_StringValue{StringValue: v}}}
}

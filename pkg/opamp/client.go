package opamp

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/collectorctrl/collectorctrl/internal/readiness"
	"github.com/collectorctrl/collectorctrl/pkg/api"
	opampclient "github.com/open-telemetry/opamp-go/client"
	opampcltypes "github.com/open-telemetry/opamp-go/client/types"
	"github.com/open-telemetry/opamp-go/protobufs"
)

type ClientConfig struct {
	Endpoint            string
	EnrollmentToken     string
	LegacySecret        string // opt-in migration only; never used after a credential is issued
	CredentialDirectory string
	TLSConfig           *tls.Config
	AgentID             string
	AgentType           api.AgentType
	Labels              map[string]string
}

// Observation is a Kubernetes API snapshot, never a runtime effective config.
// Keep this versioned wire contract in sync with the server's K8sObservation.
type Observation struct {
	Source                string      `json:"source"`
	RuntimeVerified       bool        `json:"runtime_verified"`
	ObservedAt            time.Time   `json:"observed_at"`
	ReportIntervalSeconds int64       `json:"report_interval_seconds,omitempty"`
	Namespace             string      `json:"namespace"`
	WorkloadKind          string      `json:"workload_kind"`
	WorkloadName          string      `json:"workload_name"`
	WorkloadUID           string      `json:"workload_uid"`
	CollectorContainer    string      `json:"collector_container"`
	ConfigMapName         string      `json:"configmap_name"`
	ConfigMapKey          string      `json:"configmap_key"`
	ResourceVersion       string      `json:"resource_version"`
	ConfigHash            string      `json:"config_hash"`
	ConfigYAML            string      `json:"config_yaml,omitempty"`
	DesiredPods           int         `json:"desired_pods"`
	ReadyPods             int         `json:"ready_pods"`
	Pods                  []PodHealth `json:"pods"`

	// Fields below are additive and optional (operator v0.7+). They are
	// inferred from the mounted configuration, never from the running process.
	Discovery   string       `json:"discovery,omitempty"`    // "label" or "collectormonitor"
	Stack       string       `json:"stack,omitempty"`        // group of workloads deployed together
	StackSource string       `json:"stack_source,omitempty"` // annotation, otel-operator, helm-release, argocd-app, standalone
	Role        string       `json:"role,omitempty"`         // agent, gateway, cluster-receiver, all-in-one, unknown
	RoleSource  string       `json:"role_source,omitempty"`  // inferred or annotation
	RoleReasons []string     `json:"role_reasons,omitempty"`
	Receivers   []string     `json:"receivers,omitempty"` // component IDs used in pipelines
	Processors  []string     `json:"processors,omitempty"`
	Exporters   []string     `json:"exporters,omitempty"`
	Exports     []ExportEdge `json:"exports,omitempty"`
	ConfigError string       `json:"config_error,omitempty"` // set when the config could not be analysed
}

// ExportEdge is where one exporter sends data. Target is workload, service,
// external, local or unresolved.
type ExportEdge struct {
	Exporter  string `json:"exporter"`
	Endpoint  string `json:"endpoint"`
	Target    string `json:"target"`
	Namespace string `json:"namespace,omitempty"`
	Kind      string `json:"kind,omitempty"`
	Name      string `json:"name,omitempty"`
	Service   string `json:"service,omitempty"`
}
type PodHealth struct {
	UID         string `json:"uid,omitempty"`
	Terminating bool   `json:"terminating,omitempty"`
	Name        string `json:"name"`
	Node        string `json:"node"`
	Ready       bool   `json:"ready"`
	Phase       string `json:"phase"`
	Image       string `json:"image"`
	Restarts    int32  `json:"restarts"`
	Reason      string `json:"reason,omitempty"`
}

type Client struct {
	config          ClientConfig
	client          opampclient.OpAMPClient
	mu              sync.RWMutex
	connected       bool
	credential      string
	authError       error
	connectionError error
	ackPending      bool
	sendMu          sync.Mutex
	stopOnce        sync.Once
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
	endpoint, err := ApprovedEndpoint(c.config.Endpoint, "")
	if err != nil {
		return err
	}
	c.config.Endpoint = endpoint
	if err := c.prepareCredentials(); err != nil {
		return err
	}
	c.client = opampclient.NewWebSocket(nil)
	if err := c.client.SetCustomCapabilities(&protobufs.CustomCapabilities{Capabilities: []string{"collectorctrl", "credentials"}}); err != nil {
		return err
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
		InstanceUid: opampcltypes.InstanceUid(c.instanceUID()), HeaderFunc: c.authHeaders,
		Capabilities: protobufs.AgentCapabilities_AgentCapabilities_ReportsHealth | protobufs.AgentCapabilities_AgentCapabilities_ReportsStatus,
		Callbacks: opampcltypes.Callbacks{
			OnConnect: func(context.Context) {
				c.setConnected(true)
				c.mu.Lock()
				c.ackPending = c.credential != ""
				c.mu.Unlock()
				_ = c.sendCredentialAck()
			},
			OnConnectFailed: func(_ context.Context, err error) {
				c.mu.Lock()
				c.connected = false
				c.connectionError = err
				c.mu.Unlock()
			},
			OnError: func(context.Context, *protobufs.ServerErrorResponse) { c.setConnected(false) },
			OnCommand: func(context.Context, *protobufs.ServerToAgentCommand) error {
				return fmt.Errorf("observation mode does not accept commands")
			},
			OnOpampConnectionSettings: func(context.Context, *protobufs.OpAMPConnectionSettings) error {
				return fmt.Errorf("endpoint and credentials are controlled by the operator administrator")
			},
			OnMessage: func(_ context.Context, msg *opampcltypes.MessageData) {
				if cm := msg.CustomMessage; cm != nil && cm.Capability == "credentials" && cm.Type == "agent_credential" {
					if err := c.saveCredential(string(cm.Data)); err != nil {
						c.mu.Lock()
						c.authError = fmt.Errorf("persist enrollment credential: %w", err)
						c.mu.Unlock()
						return
					}
					c.mu.Lock()
					c.ackPending = true
					c.mu.Unlock()
					_ = c.sendCredentialAck()
					return
				}
				if msg.CustomMessage != nil && msg.CustomMessage.Capability == "collectorctrl" && msg.CustomMessage.Type == "emergency" {
					_, _ = c.client.SendCustomMessage(&protobufs.CustomMessage{Capability: "collectorctrl", Type: "emergency_ack", Data: []byte("emergency_ack:false:operator is observation-only")})
				}
			},
		},
	})
}
func (c *Client) setConnected(v bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.connected = v
	if v {
		c.connectionError = nil
	}
}
func (c *Client) Connected() bool { c.mu.RLock(); defer c.mu.RUnlock(); return c.connected }
func (c *Client) ConnectionError() error {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.connectionError
}
func (c *Client) Stop() {
	c.stopOnce.Do(func() {
		c.setConnected(false)
		if c.client != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = c.client.Stop(ctx)
		}
	})
}

func (c *Client) sendCredentialAck() error {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	c.mu.RLock()
	pending := c.ackPending
	c.mu.RUnlock()
	if !pending {
		return nil
	}
	_, err := c.client.SendCustomMessage(&protobufs.CustomMessage{Capability: "credentials", Type: "agent_credential_ack"})
	if err == nil {
		c.mu.Lock()
		c.ackPending = false
		c.mu.Unlock()
	}
	return err
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
	c.mu.RLock()
	authError := c.authError
	c.mu.RUnlock()
	if authError != nil {
		return authError
	}
	if err := c.sendCredentialAck(); err != nil {
		if errors.Is(err, opampcltypes.ErrCustomMessagePending) {
			return nil
		}
		return err
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

// ClusterInventory is the cluster-scoped snapshot sent on the operator's
// cluster connection (collectorctrl/k8s_cluster_inventory_v1). It lists
// workloads that look like collectors from metadata only (image names); no
// configuration is read from workloads that have not opted in.
type ClusterInventory struct {
	Source                string               `json:"source"` // always "kubernetes-api"
	ObservedAt            time.Time            `json:"observed_at"`
	ReportIntervalSeconds int64                `json:"report_interval_seconds"`
	ClusterID             string               `json:"cluster_id"`
	ClusterName           string               `json:"cluster_name,omitempty"`
	Collectors            []CollectorCandidate `json:"collectors"`
	Truncated             bool                 `json:"truncated,omitempty"`
}

// CollectorCandidate is one collector-looking workload. OptedIn and Monitored
// say whether it is already observed (label or CollectorMonitor).
type CollectorCandidate struct {
	Namespace   string `json:"namespace"`
	Kind        string `json:"kind"`
	Name        string `json:"name"`
	UID         string `json:"uid"`
	Container   string `json:"container"`
	Image       string `json:"image"`
	Stack       string `json:"stack,omitempty"`
	StackSource string `json:"stack_source,omitempty"`
	OptedIn     bool   `json:"opted_in,omitempty"`
	Monitored   bool   `json:"monitored,omitempty"`
}

// ReportInventory sends a cluster inventory snapshot. It is a no-op while the
// transport is disconnected.
func (c *Client) ReportInventory(inv ClusterInventory) error {
	return c.sendClusterMessage("k8s_cluster_inventory_v1", inv,
		fmt.Sprintf("Cluster inventory: %d collector-like workloads", len(inv.Collectors)))
}

// ReportReadiness sends an instrumentation readiness report. Health is left
// to the inventory, which reports every minute.
func (c *Client) ReportReadiness(rep InstrumentationReadiness) error {
	return c.sendClusterMessage("k8s_instrumentation_readiness_v1", rep, "")
}

func (c *Client) sendClusterMessage(msgType string, body any, health string) error {
	if c.client == nil {
		return fmt.Errorf("client not started")
	}
	c.mu.RLock()
	authError := c.authError
	c.mu.RUnlock()
	if authError != nil {
		return authError
	}
	if err := c.sendCredentialAck(); err != nil {
		if errors.Is(err, opampcltypes.ErrCustomMessagePending) {
			return nil
		}
		return err
	}
	if health != "" {
		if err := c.client.SetHealth(&protobufs.ComponentHealth{Healthy: true, Status: health}); err != nil {
			return err
		}
	}
	if !c.Connected() {
		return nil
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	_, err = c.client.SendCustomMessage(&protobufs.CustomMessage{Capability: "collectorctrl", Type: msgType, Data: payload})
	if errors.Is(err, opampcltypes.ErrCustomMessagePending) {
		return nil
	}
	return err
}

// InstrumentationReadiness is the cluster-scoped readiness report
// (collectorctrl/k8s_instrumentation_readiness_v1). It is derived from
// Kubernetes objects only: RegistryChecked is always false, so variables baked
// into images are unknown, and values from Secrets are never read.
type InstrumentationReadiness struct {
	Source                string               `json:"source"` // always "kubernetes-api"
	ObservedAt            time.Time            `json:"observed_at"`
	ReportIntervalSeconds int64                `json:"report_interval_seconds"`
	ClusterID             string               `json:"cluster_id"`
	ClusterName           string               `json:"cluster_name,omitempty"`
	RegistryChecked       bool                 `json:"registry_checked"`
	InjectorsChecked      bool                 `json:"injectors_checked"`
	Injectors             []readiness.Injector `json:"injectors"`
	Workloads             []ReadinessWorkload  `json:"workloads"`
	Summary               map[string]int       `json:"summary"` // workloads per verdict, before truncation
	Truncated             bool                 `json:"truncated,omitempty"`
}

// ReadinessWorkload is one application workload and its containers.
type ReadinessWorkload struct {
	Namespace   string                `json:"namespace"`
	Kind        string                `json:"kind"`
	Name        string                `json:"name"`
	UID         string                `json:"uid"`
	Stack       string                `json:"stack,omitempty"`
	StackSource string                `json:"stack_source,omitempty"`
	Pod         string                `json:"pod,omitempty"` // running pod inspected for injected agents
	Verdict     string                `json:"verdict"`
	Containers  []readiness.Container `json:"containers"`
}

func keyVal(k, v string) *protobufs.KeyValue {
	return &protobufs.KeyValue{Key: k, Value: &protobufs.AnyValue{Value: &protobufs.AnyValue_StringValue{StringValue: v}}}
}

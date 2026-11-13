package mcpgw

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/rnikadi/genaiopengateway/internal/config"
	"github.com/rnikadi/genaiopengateway/internal/store"
)

// Pins keeps tool pins. *store.Store implements it; without a store the
// gateway keeps pins in memory.
type Pins interface {
	ListPins(ctx context.Context, server string) ([]*store.MCPToolPin, error)
	InsertPin(ctx context.Context, p *store.MCPToolPin) error
	UpdatePinHash(ctx context.Context, p *store.MCPToolPin) error
	ApprovePin(ctx context.Context, server, tool, hash string) (*store.MCPToolPin, error)
}

// toolHash returns the SHA-256 of a tool's definition as canonical JSON,
// and that JSON. _meta is left out: it carries no instructions for the
// model and may change on every listing.
func toolHash(t *mcp.Tool) (string, json.RawMessage) {
	tt := *t
	tt.Meta = nil
	// Maps marshal with sorted keys, so equal definitions hash equally.
	var canon any
	raw, _ := json.Marshal(&tt)
	_ = json.Unmarshal(raw, &canon)
	raw, _ = json.Marshal(canon)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), raw
}

// applyPins decides which tools of a fresh catalog are hidden: tools whose
// definition differs from the approved one, and in strict mode tools never
// approved. It records first sights and changes in the pin store.
func (g *Gateway) applyPins(ctx context.Context, u *upstream, c *catalog) error {
	c.hidden = map[string]bool{}
	mode := u.cfg.PinTools
	if mode == config.PinOff {
		return nil
	}
	pins, err := g.pins.ListPins(ctx, u.name)
	if err != nil {
		return err
	}
	byTool := make(map[string]*store.MCPToolPin, len(pins))
	for _, p := range pins {
		byTool[p.Tool] = p
	}
	for _, t := range c.Tools {
		hash, def := toolHash(t)
		p := byTool[t.Name]
		switch {
		case p == nil:
			p = &store.MCPToolPin{Server: u.name, Tool: t.Name, Hash: hash, Definition: def}
			if mode == config.PinOn {
				p.ApprovedHash = hash
			}
			if err := g.pins.InsertPin(ctx, p); err != nil {
				return err
			}
			if mode != config.PinOn {
				g.opts.Log.Warn("new MCP tool hidden until approved", "server", u.name, "tool", t.Name, "hash", hash)
				g.pending(u.name, t.Name, hash, false)
			}
		case p.Hash != hash:
			approved := p.ApprovedHash == hash
			p.Hash, p.Definition = hash, def
			if err := g.pins.UpdatePinHash(ctx, p); err != nil {
				return err
			}
			if !approved {
				g.opts.Log.Warn("MCP tool definition changed; hidden until approved", "server", u.name, "tool", t.Name, "hash", hash)
				g.audit(ctx, "mcp_tool.changed", u.name+"/"+t.Name, map[string]string{"hash": hash, "approved_hash": p.ApprovedHash})
				g.pending(u.name, t.Name, hash, true)
			}
		}
		if p.ApprovedHash != hash {
			c.hidden[t.Name] = true
		}
	}
	return nil
}

func (g *Gateway) pending(server, tool, hash string, changed bool) {
	if g.opts.OnPending != nil {
		go g.opts.OnPending(server, tool, hash, changed)
	}
}

func (g *Gateway) audit(ctx context.Context, action, resource string, details any) {
	if g.opts.Audit == nil {
		return
	}
	raw, _ := json.Marshal(details)
	err := g.opts.Audit(ctx, &store.AuditEntry{Actor: "gateway", Action: action, ResourceType: "mcp_tool", ResourceID: resource, Details: raw})
	if err != nil {
		g.opts.Log.Error("writing audit entry failed", "action", action, "resource", resource, "err", err)
	}
}

// Approve approves a tool's current definition, identified by its hash,
// and shows the tool at once on this replica. Other replicas show it
// after their next refresh.
func (g *Gateway) Approve(ctx context.Context, server, tool, hash string) (*store.MCPToolPin, error) {
	g.mu.Lock()
	u := g.upstreams[server]
	g.mu.Unlock()
	if u == nil {
		return nil, store.ErrNotFound
	}
	p, err := g.pins.ApprovePin(ctx, server, tool, hash)
	if err != nil {
		return nil, err
	}
	if prev := u.catalog.Load(); prev != nil {
		c := *prev
		if err := g.applyPins(ctx, u, &c); err != nil {
			return nil, err
		}
		c.seal()
		u.catalog.Store(&c)
		g.opts.Metrics.SetMCPPendingTools(u.name, len(c.hidden))
		g.rebuild()
	}
	return p, nil
}

// ServerStatus describes an upstream server for the admin API.
type ServerStatus struct {
	Name            string         `json:"name"`
	URL             string         `json:"url"`
	Up              bool           `json:"up"`
	ProtocolVersion string         `json:"protocol_version,omitempty"`
	PinTools        config.PinMode `json:"pin_tools"`
	UserAuth        bool           `json:"user_auth"`
	Tools           int            `json:"tools"`
	HiddenTools     int            `json:"hidden_tools"`
	Prompts         int            `json:"prompts"`
	Resources       int            `json:"resources"`
	RefreshedAt     *time.Time     `json:"refreshed_at"`
}

// Servers reports every upstream server, by name.
func (g *Gateway) Servers() []ServerStatus {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := []ServerStatus{}
	for name, u := range g.upstreams {
		st := ServerStatus{Name: name, URL: u.cfg.URL, Up: u.up.Load(), PinTools: u.cfg.PinTools, UserAuth: u.cfg.UserAuth != nil}
		if c := u.catalog.Load(); c != nil {
			st.ProtocolVersion, st.Tools, st.HiddenTools = c.protocolVersion, len(c.Tools), len(c.hidden)
			st.Prompts, st.Resources = len(c.Prompts), len(c.Resources)+len(c.Templates)
			st.RefreshedAt = &c.refreshedAt
		}
		out = append(out, st)
	}
	slices.SortFunc(out, func(a, b ServerStatus) int {
		if a.Name < b.Name {
			return -1
		}
		return 1
	})
	return out
}

// ToolPin is a pin with its status, for the admin API.
type ToolPin struct {
	*store.MCPToolPin
	Status string `json:"status"`
}

// Tools lists pins, optionally of one server and in one status.
func (g *Gateway) Tools(ctx context.Context, server, status string) ([]ToolPin, error) {
	pins, err := g.pins.ListPins(ctx, server)
	if err != nil {
		return nil, err
	}
	out := []ToolPin{}
	for _, p := range pins {
		if st := p.Status(); status == "" || st == status {
			out = append(out, ToolPin{p, st})
		}
	}
	return out, nil
}

// memPins keeps pins in memory, for gateways without a store. Every
// restart is a first sight.
type memPins struct {
	mu   sync.Mutex
	pins map[[2]string]store.MCPToolPin
}

func newMemPins() *memPins { return &memPins{pins: map[[2]string]store.MCPToolPin{}} }

func (m *memPins) ListPins(_ context.Context, server string) ([]*store.MCPToolPin, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*store.MCPToolPin
	for k, p := range m.pins {
		if server == "" || k[0] == server {
			out = append(out, &p)
		}
	}
	slices.SortFunc(out, func(a, b *store.MCPToolPin) int {
		if a.Server+"\x00"+a.Tool < b.Server+"\x00"+b.Tool {
			return -1
		}
		return 1
	})
	return out, nil
}

func (m *memPins) InsertPin(_ context.Context, p *store.MCPToolPin) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := [2]string{p.Server, p.Tool}
	if _, ok := m.pins[k]; !ok {
		v := *p
		v.UpdatedAt = time.Now().UTC()
		m.pins[k] = v
	}
	return nil
}

func (m *memPins) UpdatePinHash(_ context.Context, p *store.MCPToolPin) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := [2]string{p.Server, p.Tool}
	if v, ok := m.pins[k]; ok {
		v.Hash, v.Definition, v.UpdatedAt = p.Hash, p.Definition, time.Now().UTC()
		m.pins[k] = v
	}
	return nil
}

func (m *memPins) ApprovePin(_ context.Context, server, tool, hash string) (*store.MCPToolPin, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := [2]string{server, tool}
	v, ok := m.pins[k]
	if !ok {
		return nil, store.ErrNotFound
	}
	if v.Hash != hash {
		return nil, fmt.Errorf("%w: the definition of %s/%s changed since it was shown; its hash is now %s", store.ErrConflict, server, tool, v.Hash)
	}
	v.ApprovedHash, v.UpdatedAt = hash, time.Now().UTC()
	m.pins[k] = v
	return &v, nil
}

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"log"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/alternayte/kiln/internal/hostca"
	"github.com/alternayte/kiln/internal/network"
	"github.com/alternayte/kiln/internal/store"
)

// maxCredentials bounds the bindings of one sandbox. Each one adds a guard rule
// and can add a leaf certificate.
const maxCredentials = 16

var (
	bindingName = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
	headerName  = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]{1,256}$")
	methodName  = regexp.MustCompile(`^[A-Z]{1,32}$`)
)

// reservedHeaders frame a request. A key in one of them would change where the
// request goes or how it is read.
var reservedHeaders = map[string]bool{
	"host": true, "content-length": true, "transfer-encoding": true, "connection": true,
	"upgrade": true, "te": true, "trailer": true, "keep-alive": true,
	"proxy-connection": true, "proxy-authenticate": true, "proxy-authorization": true,
}

// CredentialInput is one binding a caller sends.
type CredentialInput struct {
	Name    string
	Host    string
	Header  string
	Value   string
	Path    string
	Methods []string
}

// sealCredentials validates a list against the policy that covers it, and seals
// each value. A caller's host must fall inside egress_allow, because a binding
// must never point at a closed host.
func (m *Manager) sealCredentials(list []CredentialInput, allow []string) ([]store.CredentialBinding, error) {
	if len(list) > maxCredentials {
		return nil, invalidf("a sandbox holds at most %d credentials", maxCredentials)
	}
	names := map[string]bool{}
	out := make([]store.CredentialBinding, 0, len(list))
	for i, c := range list {
		if !bindingName.MatchString(c.Name) {
			return nil, invalidf("credentials[%d]: name must be letters, digits, dot, dash or underscore", i)
		}
		if names[c.Name] {
			return nil, invalidf("credentials[%d]: name %q appears twice", i, c.Name)
		}
		names[c.Name] = true
		host := strings.ToLower(strings.TrimSuffix(c.Host, "."))
		if strings.HasPrefix(host, "*") || network.ValidateAllow([]string{host}) != nil {
			return nil, invalidf("credentials[%d]: host must be one exact name", i)
		}
		if !network.Covers(allow, host) {
			return nil, invalidf("credentials[%d]: egress_allow does not cover %s", i, host)
		}
		if !headerName.MatchString(c.Header) || reservedHeaders[strings.ToLower(c.Header)] {
			return nil, invalidf("credentials[%d]: header %q cannot carry a credential", i, c.Header)
		}
		if c.Value == "" {
			return nil, invalidf("credentials[%d]: value is required", i)
		}
		for _, r := range c.Value {
			if r < 0x20 || r == 0x7f {
				return nil, invalidf("credentials[%d]: value holds a control character", i)
			}
		}
		if c.Path != "" && !network.ValidBindingPath(c.Path) {
			return nil, invalidf("credentials[%d]: path must be a clean absolute path", i)
		}
		for _, method := range c.Methods {
			if !methodName.MatchString(method) {
				return nil, invalidf("credentials[%d]: method %q is not an uppercase HTTP method", i, method)
			}
		}
		sealed, err := store.Seal(m.cfg.SealKey, c.Value)
		if err != nil {
			return nil, err
		}
		out = append(out, store.CredentialBinding{
			Name: c.Name, Host: host, Header: c.Header, ValueSealed: sealed, Path: c.Path, Methods: c.Methods,
		})
	}
	return out, nil
}

// requireTrust refuses a binding on a template whose guests do not trust the CA.
func requireTrust(tpl store.Template) error {
	if !tpl.TrustsCA {
		return fmt.Errorf("%w: template %q was built before credential bindings, so its guests do not trust the CA: rebuild the template", store.ErrConflict, tpl.Name)
	}
	return nil
}

// SetCredentials replaces every binding of one sandbox. A sleeping sandbox takes
// the change without a wake, and the wake applies it.
func (m *Manager) SetCredentials(ctx context.Context, id string, list []CredentialInput) (store.Sandbox, error) {
	unlock := m.rlockOps(id)
	defer unlock()
	m.egressMu.Lock()
	defer m.egressMu.Unlock()
	row, err := m.egressRow(ctx, id)
	if err != nil {
		return store.Sandbox{}, err
	}
	if row.EgressObserve {
		return store.Sandbox{}, fmt.Errorf("%w: a sandbox in observe mode cannot hold a credential", store.ErrConflict)
	}
	tpl, err := m.cfg.Store.GetTemplate(ctx, row.TemplateName)
	if err != nil {
		return store.Sandbox{}, err
	}
	if len(list) > 0 {
		if err := requireTrust(tpl); err != nil {
			return store.Sandbox{}, err
		}
	}
	var att *network.Attachment
	if row.State == store.SandboxRunning {
		m.mu.Lock()
		att = m.attrs[row.ID]
		m.mu.Unlock()
		if att == nil {
			return store.Sandbox{}, store.ErrConflict
		}
		if !att.HasGuard() {
			return store.Sandbox{}, fmt.Errorf("%w: this sandbox has no egress guard until its next wake", store.ErrConflict)
		}
	}
	sealed, err := m.sealCredentials(list, row.Egress(tpl))
	if err != nil {
		return store.Sandbox{}, err
	}
	before, err := m.cfg.Store.Credentials(ctx, row.ID)
	if err != nil {
		return store.Sandbox{}, err
	}
	if err := m.cfg.Store.SetCredentials(ctx, row.ID, sealed); err != nil {
		return store.Sandbox{}, err
	}
	m.event(ctx, row.ID, row.State, row.State, "credentials changed", m.now())
	if att == nil {
		return row, nil
	}
	policy, err := m.policyOf(ctx, row, tpl)
	if err != nil {
		return store.Sandbox{}, err
	}
	if err := att.SetBindings(ctx, policy, changedHosts(before, sealed)); err != nil {
		return store.Sandbox{}, err
	}
	m.touch(ctx, row)
	return row, nil
}

// changedHosts lists each host that gained or lost its last binding.
func changedHosts(before, after []store.CredentialBinding) []string {
	had, has := map[string]bool{}, map[string]bool{}
	for _, b := range before {
		had[b.Host] = true
	}
	for _, b := range after {
		has[b.Host] = true
	}
	var out []string
	for host := range had {
		if !has[host] {
			out = append(out, host)
		}
	}
	for host := range has {
		if !had[host] {
			out = append(out, host)
		}
	}
	sort.Strings(out)
	return out
}

// Credentials returns the bindings of one sandbox. Only ever show a caller the
// fields next to ValueSealed.
func (m *Manager) Credentials(ctx context.Context, id string) ([]store.CredentialBinding, error) {
	return m.cfg.Store.Credentials(ctx, id)
}

// PolicyOf is the whole network policy of one sandbox row, with its credentials
// opened and a leaf for each bound host. A failure leaves the credentials out:
// a sandbox with no key fails on the provider and leaks nothing.
func (m *Manager) PolicyOf(ctx context.Context, row store.Sandbox, tpl store.Template) network.Policy {
	policy, err := m.policyOf(ctx, row, tpl)
	if err != nil {
		log.Printf("sandbox: %s: credentials: %v", row.ID, err)
		return network.Policy{Allow: row.Egress(tpl), Observe: row.EgressObserve}
	}
	return policy
}

func (m *Manager) policyOf(ctx context.Context, row store.Sandbox, tpl store.Template) (network.Policy, error) {
	policy := network.Policy{Allow: row.Egress(tpl), Observe: row.EgressObserve}
	rows, err := m.cfg.Store.Credentials(ctx, row.ID)
	if err != nil || len(rows) == 0 {
		return policy, err
	}
	bindings, leaves, err := m.openCredentials(ctx, rows, tpl.TenantID)
	if err != nil {
		return policy, err
	}
	policy.Bindings, policy.Leaves = bindings, leaves
	return policy, nil
}

func (m *Manager) openCredentials(ctx context.Context, rows []store.CredentialBinding, tenant string) ([]network.Binding, []network.Leaf, error) {
	ca, err := hostca.TenantCA(ctx, m.cfg.Store, m.cfg.SealKey, tenant)
	if err != nil {
		return nil, nil, err
	}
	var (
		bindings []network.Binding
		leaves   []network.Leaf
		issued   = map[string]bool{}
	)
	for _, row := range rows {
		value, err := store.Unseal(m.cfg.SealKey, row.ValueSealed)
		if err != nil {
			return nil, nil, err
		}
		bindings = append(bindings, network.Binding{
			Name: row.Name, Host: row.Host, Header: row.Header, Value: value, Path: row.Path, Methods: row.Methods,
		})
		if issued[row.Host] {
			continue
		}
		issued[row.Host] = true
		cert, key, err := hostca.IssueLeaf(m.cfg.SealKey, ca, row.Host)
		if err != nil {
			return nil, nil, err
		}
		leaves = append(leaves, network.Leaf{Host: row.Host, Cert: cert, Key: key})
	}
	return bindings, leaves, nil
}

// checkEgressKeepsBindings refuses a policy that closes a bound host.
func (m *Manager) checkEgressKeepsBindings(ctx context.Context, id string, allow []string) error {
	rows, err := m.cfg.Store.Credentials(ctx, id)
	if err != nil {
		return err
	}
	for _, row := range rows {
		if !network.Covers(allow, row.Host) {
			return fmt.Errorf("%w: credential %q is bound to %s: replace the credentials first", store.ErrConflict, row.Name, row.Host)
		}
	}
	return nil
}

// recordRequests stores the request log of one sandbox.
func (m *Manager) recordRequests(id string, rows []network.RequestRecord, dropped int64) {
	m.cfg.Metrics.RequestsDropped.Add(uint64(max(dropped, 0)))
	out := make([]store.EgressRequest, 0, len(rows))
	for _, row := range rows {
		out = append(out, store.EgressRequest{
			Method: row.Method, Host: row.Host, Path: row.Path, Status: row.Status,
			Binding: row.Binding, At: time.Unix(row.At, 0).UTC(),
		})
	}
	if err := m.cfg.Store.RecordEgressRequests(context.Background(), id, out, dropped); err != nil {
		log.Printf("sandbox: %s: request log: %v", id, err)
	}
}

// EgressRequests returns the request log of one sandbox, with the retention of
// its egress record.
func (m *Manager) EgressRequests(ctx context.Context, id string) (store.EgressRequests, error) {
	out, err := m.cfg.Store.ReadEgressRequests(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		row, rowErr := m.cfg.Store.GetSandbox(ctx, id)
		if rowErr != nil {
			return store.EgressRequests{}, rowErr
		}
		out, err = store.EgressRequests{DestroyedAt: row.DestroyedAt}, nil
	}
	if err != nil {
		return store.EgressRequests{}, err
	}
	if out.DestroyedAt != nil && m.now().Sub(*out.DestroyedAt) > EgressRetention {
		return store.EgressRequests{}, store.ErrNotFound
	}
	return out, nil
}

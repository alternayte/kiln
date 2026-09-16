// Package apispec is the one description of the Kiln API. The OpenAPI
// document, the MCP tools and the agent documentation come from it, so the
// three never drift from each other, and a check compares it with the routes
// the host actually serves.
package apispec

import "sort"

// Field is one value in a request body or a path.
type Field struct {
	Name     string
	Type     string // string, integer, boolean, array, object
	Items    string // element type of an array
	Required bool
	Doc      string
}

// Operation is one call a caller can make.
type Operation struct {
	// ID is the name a client method and an MCP tool take.
	ID string
	// Method and Path are the route, in the shape the host registers.
	Method string
	Path   string
	// Summary is one sentence. It reaches the agent as the tool description.
	Summary string
	// Params are the path parameters, in path order.
	Params []Field
	// Body holds the request fields. It is empty for a call with no body.
	Body []Field
	// Returns names the shape of the answer.
	Returns string
	// Status is the status a success carries.
	Status int
	// Operator marks a route only the operator token reaches. The gateway
	// never exposes one to a tenant, and no tool is made for it.
	Operator bool
	// Streams marks a route that can stream, so a client reads it as it
	// arrives instead of buffering.
	Streams bool
	// Terminal marks a WebSocket upgrade. OpenAPI 3.1 cannot describe one,
	// so the document and the generated clients leave it out, and the
	// handwritten SDK layer carries it. The route is still described here,
	// so the host cannot serve a route no document names.
	Terminal bool
}

// Operations is the whole API. A route the host serves and this list does not
// name fails checks/openapi.sh, and so does the reverse.
func Operations() []Operation {
	ops := []Operation{
		{
			ID: "health", Method: "GET", Path: "/v1/health", Status: 200,
			Summary: "Report whether the host can run sandboxes.",
			Returns: "Health",
		},
		{
			ID: "events", Method: "GET", Path: "/v1/events", Status: 200, Streams: true,
			Summary: "Read the event stream of this tenant.",
			Returns: "EventList",
		},
		{
			ID: "listTemplates", Method: "GET", Path: "/v1/templates", Status: 200,
			Summary: "List the templates of this tenant.",
			Returns: "TemplateList",
		},
		{
			ID: "createTemplate", Method: "POST", Path: "/v1/templates", Status: 202,
			Summary: "Build a template from an OCI image. The build runs after the answer.",
			Body: []Field{
				{Name: "name", Type: "string", Required: true, Doc: "Name of the template inside this tenant."},
				{Name: "image", Type: "string", Required: true, Doc: "OCI image reference, for example docker.io/library/python:3.12-slim."},
				{Name: "vcpus", Type: "integer", Doc: "Virtual CPUs of every sandbox from this template."},
				{Name: "memory_mb", Type: "integer", Doc: "Memory of every sandbox from this template."},
				{Name: "disk_mb", Type: "integer", Doc: "Disk of every sandbox from this template."},
				{Name: "setup", Type: "array", Items: "string", Doc: "Commands that run once while the template builds."},
				{Name: "egress_allow", Type: "array", Items: "string", Required: true,
					Doc: "Hostnames a sandbox may reach. An empty list allows no egress."},
				{Name: "ttl_seconds", Type: "integer",
					Doc: "Seconds until the host deletes this template with its snapshot and its sandboxes. A preview environment sets one, because nothing comes back to delete it."},
				{Name: "start", Type: "array", Items: "string",
					Doc: "Command that runs the application. Kiln ignores what the image says to run, so a template that serves an application names this. It runs once, when a sandbox restores from this template."},
				{Name: "port", Type: "integer",
					Doc: "Port the start command listens on. Creating a sandbox answers only once the guest accepts a connection there. Required with start."},
			},
			Returns: "TemplateState",
		},
		{
			ID: "getTemplate", Method: "GET", Path: "/v1/templates/{name}", Status: 200,
			Summary: "Read one template.",
			Params:  []Field{{Name: "name", Type: "string", Required: true, Doc: "Name of the template."}},
			Returns: "Template",
		},
		{
			ID: "deleteTemplate", Method: "DELETE", Path: "/v1/templates/{name}", Status: 204,
			Summary: "Delete a template that has no live sandbox and no snapshot.",
			Params:  []Field{{Name: "name", Type: "string", Required: true, Doc: "Name of the template."}},
		},
		{
			ID: "listSandboxes", Method: "GET", Path: "/v1/sandboxes", Status: 200,
			Summary: "List the sandboxes of this tenant.",
			Returns: "SandboxList",
		},
		{
			ID: "createSandbox", Method: "POST", Path: "/v1/sandboxes", Status: 201,
			Summary: "Create a sandbox by restoring a template snapshot.",
			Body: []Field{
				{Name: "template", Type: "string", Required: true, Doc: "Template the sandbox restores from."},
				{Name: "lifecycle", Type: "string", Doc: "ephemeral or persistent."},
				{Name: "ttl_seconds", Type: "integer", Doc: "Seconds until the sandbox is destroyed."},
				{Name: "idle_seconds", Type: "integer", Doc: "Seconds of inactivity until the sandbox sleeps."},
				{Name: "metadata", Type: "object", Doc: "Free-form labels the caller reads back."},
				{Name: "secrets", Type: "array", Items: "string", Doc: "Names of host secrets to inject."},
			},
			Returns: "Sandbox",
		},
		{
			ID: "getSandbox", Method: "GET", Path: "/v1/sandboxes/{id}", Status: 200,
			Summary: "Read one sandbox with its published ports.",
			Params:  []Field{{Name: "id", Type: "string", Required: true, Doc: "Sandbox id."}},
			Returns: "Sandbox",
		},
		{
			ID: "deleteSandbox", Method: "DELETE", Path: "/v1/sandboxes/{id}", Status: 204,
			Summary: "Destroy a sandbox and retire its hostnames.",
			Params:  []Field{{Name: "id", Type: "string", Required: true, Doc: "Sandbox id."}},
		},
		{
			ID: "exec", Method: "POST", Path: "/v1/sandboxes/{id}/exec", Status: 200, Streams: true,
			Summary: "Run a command inside a sandbox. Accept text/event-stream to read the output as it runs.",
			Params:  []Field{{Name: "id", Type: "string", Required: true, Doc: "Sandbox id."}},
			Body: []Field{
				{Name: "cmd", Type: "array", Items: "string", Required: true, Doc: "Command and its arguments."},
				{Name: "cwd", Type: "string", Doc: "Directory the command runs in."},
				{Name: "env", Type: "object", Doc: "Extra environment variables."},
				{Name: "timeout_seconds", Type: "integer", Doc: "Seconds until the command is killed."},
			},
			Returns: "ExecResult",
		},
		{
			ID: "terminal", Method: "GET", Path: "/v1/sandboxes/{id}/terminal", Status: 101, Terminal: true,
			Summary: "Open an interactive shell in a sandbox over a WebSocket. A binary message carries terminal bytes both ways, and a text message carries {\"cols\":n,\"rows\":n}.",
			Params: []Field{
				{Name: "id", Type: "string", Required: true, Doc: "Sandbox id."},
				{Name: "cols", Type: "integer", Doc: "Terminal width at the moment the shell opens."},
				{Name: "rows", Type: "integer", Doc: "Terminal height at the moment the shell opens."},
			},
		},
		{
			ID: "snapshot", Method: "POST", Path: "/v1/sandboxes/{id}/snapshot", Status: 201,
			Summary: "Snapshot a sandbox. With stop the sandbox sleeps and keeps its hostnames.",
			Params:  []Field{{Name: "id", Type: "string", Required: true, Doc: "Sandbox id."}},
			Body:    []Field{{Name: "stop", Type: "boolean", Doc: "Stop the sandbox after the snapshot."}},
			Returns: "SnapshotCreated",
		},
		{
			ID: "fork", Method: "POST", Path: "/v1/sandboxes/{id}/fork", Status: 201,
			Summary: "Snapshot a running sandbox and restore several copies of it.",
			Params:  []Field{{Name: "id", Type: "string", Required: true, Doc: "Sandbox id."}},
			Body: []Field{
				{Name: "count", Type: "integer", Required: true, Doc: "Number of copies."},
				{Name: "allow_secret_fork", Type: "boolean", Doc: "Allow a fork of a sandbox that holds secrets."},
			},
			Returns: "SandboxList",
		},
		{
			ID: "publish", Method: "POST", Path: "/v1/sandboxes/{id}/publish", Status: 201,
			Summary: "Serve a guest port on a public hostname.",
			Params:  []Field{{Name: "id", Type: "string", Required: true, Doc: "Sandbox id."}},
			Body: []Field{
				{Name: "port", Type: "integer", Required: true, Doc: "Port inside the sandbox."},
				{Name: "visibility", Type: "string", Required: true, Doc: "public or team."},
			},
			Returns: "Published",
		},
		{
			ID: "retire", Method: "DELETE", Path: "/v1/sandboxes/{id}/publish/{port}", Status: 204,
			Summary: "Retire a published hostname. It answers 404 from then on.",
			Params: []Field{
				{Name: "id", Type: "string", Required: true, Doc: "Sandbox id."},
				{Name: "port", Type: "integer", Required: true, Doc: "Published guest port."},
			},
		},
		{
			ID: "readFile", Method: "GET", Path: "/v1/sandboxes/{id}/files/{path...}", Status: 200, Streams: true,
			Summary: "Read one file out of a sandbox.",
			Params: []Field{
				{Name: "id", Type: "string", Required: true, Doc: "Sandbox id."},
				{Name: "path", Type: "string", Required: true, Doc: "Path inside the sandbox."},
			},
			Returns: "FileBody",
		},
		{
			ID: "writeFile", Method: "PUT", Path: "/v1/sandboxes/{id}/files/{path...}", Status: 204, Streams: true,
			Summary: "Write one file into a sandbox.",
			Params: []Field{
				{Name: "id", Type: "string", Required: true, Doc: "Sandbox id."},
				{Name: "path", Type: "string", Required: true, Doc: "Path inside the sandbox."},
			},
		},
		{
			ID: "listSnapshots", Method: "GET", Path: "/v1/snapshots", Status: 200,
			Summary: "List the snapshots of this tenant.",
			Returns: "SnapshotList",
		},
		{
			ID: "getSnapshot", Method: "GET", Path: "/v1/snapshots/{id}", Status: 200,
			Summary: "Read one snapshot.",
			Params:  []Field{{Name: "id", Type: "string", Required: true, Doc: "Snapshot id."}},
			Returns: "Snapshot",
		},
		{
			ID: "deleteSnapshot", Method: "DELETE", Path: "/v1/snapshots/{id}", Status: 204,
			Summary: "Delete a snapshot that has no live sandbox.",
			Params:  []Field{{Name: "id", Type: "string", Required: true, Doc: "Snapshot id."}},
		},
		{
			ID: "restore", Method: "POST", Path: "/v1/snapshots/{id}/restore", Status: 201,
			Summary: "Restore a snapshot into one or more new sandboxes.",
			Params:  []Field{{Name: "id", Type: "string", Required: true, Doc: "Snapshot id."}},
			Body: []Field{
				{Name: "count", Type: "integer", Required: true, Doc: "Number of sandboxes."},
				{Name: "allow_secret_fork", Type: "boolean", Doc: "Allow a restore of a snapshot that holds secrets."},
			},
			Returns: "SandboxList",
		},
		{
			ID: "listRegistries", Method: "GET", Path: "/v1/registries", Status: 200,
			Summary: "List the registry credentials of this tenant. The tokens are never returned.",
			Returns: "RegistryList",
		},
		{
			ID: "putRegistry", Method: "POST", Path: "/v1/registries", Status: 201,
			Summary: "Store the credential a template build uses to pull a private image for this tenant.",
			Body: []Field{
				{Name: "host", Type: "string", Required: true, Doc: "Registry hostname, such as ghcr.io."},
				{Name: "username", Type: "string", Required: true, Doc: "Username the registry accepts."},
				{Name: "token", Type: "string", Required: true, Doc: "Token or password. It is never returned."},
			},
			Returns: "Registry",
		},
		{
			ID: "deleteRegistry", Method: "DELETE", Path: "/v1/registries/{host}", Status: 204,
			Summary: "Remove one registry credential.",
			Params:  []Field{{Name: "host", Type: "string", Required: true, Doc: "Registry hostname."}},
		},
		{
			ID: "listViewers", Method: "GET", Path: "/v1/viewers", Status: 200,
			Summary: "List the viewers of this tenant.",
			Returns: "ViewerList",
		},
		{
			ID: "deleteViewer", Method: "DELETE", Path: "/v1/viewers/{id}", Status: 204,
			Summary: "Revoke one viewer. The viewer is disabled and its sessions end.",
			Params:  []Field{{Name: "id", Type: "string", Required: true, Doc: "Viewer id."}},
		},
		{
			ID: "createViewer", Method: "POST", Path: "/v1/viewers", Status: 201,
			Summary: "Add a viewer of this tenant. A viewer opens team previews and nothing else.",
			Body: []Field{
				{Name: "email", Type: "string", Required: true, Doc: "Address the viewer signs in with."},
				{Name: "password", Type: "string", Required: true, Doc: "Password of the viewer."},
			},
			Returns: "Viewer",
		},
		// The tenant routes belong to the operator. A tenant never reaches
		// them, and no MCP tool is made for them.
		{ID: "createTenant", Method: "POST", Path: "/v1/tenants", Status: 201, Operator: true,
			Summary: "Create a tenant with its caps.", Returns: "Tenant",
			Body: []Field{
				{Name: "id", Type: "string", Required: true, Doc: "Tenant id."},
				{Name: "name", Type: "string", Doc: "Display name."},
				{Name: "max_sandboxes", Type: "integer", Doc: "Running sandboxes allowed. Zero is no limit."},
				{Name: "max_templates", Type: "integer", Doc: "Templates allowed. Zero is no limit."},
				{Name: "max_snapshot_bytes", Type: "integer", Doc: "Snapshot bytes allowed. Zero is no limit."},
			}},
		{ID: "listTenants", Method: "GET", Path: "/v1/tenants", Status: 200, Operator: true,
			Summary: "List every tenant with its caps and usage.", Returns: "TenantList"},
		{ID: "getTenant", Method: "GET", Path: "/v1/tenants/{id}", Status: 200, Operator: true,
			Summary: "Read one tenant.", Returns: "Tenant",
			Params: []Field{{Name: "id", Type: "string", Required: true, Doc: "Tenant id."}}},
		{ID: "setTenantCaps", Method: "PUT", Path: "/v1/tenants/{id}/caps", Status: 200, Operator: true,
			Summary: "Replace the caps of one tenant.", Returns: "Tenant",
			Params: []Field{{Name: "id", Type: "string", Required: true, Doc: "Tenant id."}},
			Body: []Field{
				{Name: "max_sandboxes", Type: "integer", Doc: "Running sandboxes allowed. Zero is no limit."},
				{Name: "max_templates", Type: "integer", Doc: "Templates allowed. Zero is no limit."},
				{Name: "max_snapshot_bytes", Type: "integer", Doc: "Snapshot bytes allowed. Zero is no limit."},
			}},
		{ID: "deleteTenant", Method: "DELETE", Path: "/v1/tenants/{id}", Status: 204, Operator: true,
			Summary: "Delete a tenant that owns nothing.",
			Params:  []Field{{Name: "id", Type: "string", Required: true, Doc: "Tenant id."}}},
	}
	sort.SliceStable(ops, func(i, j int) bool {
		if ops[i].Path == ops[j].Path {
			return ops[i].Method < ops[j].Method
		}
		return ops[i].Path < ops[j].Path
	})
	return ops
}

// Route is the "METHOD /path" form the host registers.
func (o Operation) Route() string { return o.Method + " " + o.Path }

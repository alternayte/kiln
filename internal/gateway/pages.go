package gateway

import (
	"html/template"
	"net/http"
)

// The sign-in page and the consent page are the two screens the OAuth flow
// needs. They are server-rendered, with no build step and no framework, like
// the status page of the host. A later gateway UI replaces them by pointing
// KILN_LOGIN_PATH and KILN_CONSENT_PATH at its own screens.

// signIn asks for the address and the password, then returns the browser to
// the consent page of the same authorization request.
func (s *Server) signIn(w http.ResponseWriter, r *http.Request) {
	renderPage(w, signInPage, pageData{
		RequestID:  r.URL.Query().Get("request_id"),
		AuthPrefix: s.authPrefix(),
	})
}

// consent shows what the client asks for, and posts the decision.
func (s *Server) consent(w http.ResponseWriter, r *http.Request) {
	renderPage(w, consentPage, pageData{
		RequestID:  r.URL.Query().Get("request_id"),
		AuthPrefix: s.authPrefix(),
	})
}

type pageData struct {
	RequestID  string
	AuthPrefix string
}

func (s *Server) authPrefix() string {
	if s.AuthPrefix == "" {
		return "/auth"
	}
	if s.AuthPrefix[len(s.AuthPrefix)-1] == '/' {
		return s.AuthPrefix[:len(s.AuthPrefix)-1]
	}
	return s.AuthPrefix
}

func renderPage(w http.ResponseWriter, page *template.Template, data pageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	// The pages carry their own script and nothing else, so nothing outside
	// this origin can run here.
	w.Header().Set("Content-Security-Policy",
		"default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; connect-src 'self'; form-action 'self'")
	_ = page.Execute(w, data)
}

const pageStyle = `<style>
:root { color-scheme: light dark; }
body { font: 16px/1.5 system-ui, sans-serif; margin: 0; display: grid; place-items: center; min-height: 100vh; }
main { width: min(28rem, 90vw); padding: 2rem; }
h1 { font-size: 1.25rem; margin: 0 0 .25rem; }
p { margin: .25rem 0 1.5rem; opacity: .75; }
label { display: block; margin-bottom: 1rem; }
input { width: 100%; padding: .6rem; font: inherit; box-sizing: border-box; }
button { padding: .6rem 1.2rem; font: inherit; cursor: pointer; }
.row { display: flex; gap: .75rem; }
ul { padding-left: 1.1rem; }
.error { color: #b00020; min-height: 1.5rem; }
</style>`

var signInPage = template.Must(template.New("sign-in").Parse(`<!doctype html>
<html lang="en"><meta charset="utf-8"><title>Sign in to Kiln</title>
<meta name="viewport" content="width=device-width, initial-scale=1">` + pageStyle + `
<main>
  <h1>Sign in to Kiln</h1>
  <p>An application asked for access to your sandboxes.</p>
  <form id="form">
    <label>Email<input type="email" name="email" autocomplete="username" required></label>
    <label>Password<input type="password" name="password" autocomplete="current-password" required></label>
    <p class="error" id="error"></p>
    <button type="submit">Sign in</button>
  </form>
</main>
<script>
const requestId = {{.RequestID}};
const prefix = {{.AuthPrefix}};
document.getElementById("form").addEventListener("submit", async (event) => {
  event.preventDefault();
  const form = new FormData(event.target);
  const error = document.getElementById("error");
  error.textContent = "";
  const response = await fetch(prefix + "/sign-in/email", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ email: form.get("email"), password: form.get("password") }),
  });
  if (!response.ok) {
    const body = await response.json().catch(() => ({}));
    error.textContent = body?.error?.message || "That email and password do not match.";
    return;
  }
  window.location.href = requestId
    ? prefix + "/consent?request_id=" + encodeURIComponent(requestId)
    : prefix + "/consent";
});
</script>
</html>`))

var consentPage = template.Must(template.New("consent").Parse(`<!doctype html>
<html lang="en"><meta charset="utf-8"><title>Authorize</title>
<meta name="viewport" content="width=device-width, initial-scale=1">` + pageStyle + `
<main>
  <h1 id="title">Authorize</h1>
  <p id="summary">Reading the request.</p>
  <ul id="scopes"></ul>
  <p class="error" id="error"></p>
  <div class="row" id="buttons" hidden>
    <button id="approve">Allow</button>
    <button id="deny">Refuse</button>
  </div>
</main>
<script>
const requestId = {{.RequestID}};
const prefix = {{.AuthPrefix}};
const error = document.getElementById("error");

async function load() {
  if (!requestId) {
    error.textContent = "This page opens from an application, and it carries no request.";
    return;
  }
  const response = await fetch(prefix + "/oauth2/request?request_id=" + encodeURIComponent(requestId));
  if (response.status === 401) {
    window.location.href = prefix + "/sign-in?request_id=" + encodeURIComponent(requestId);
    return;
  }
  if (!response.ok) {
    error.textContent = "This authorization request is unknown or spent. Start again from the application.";
    return;
  }
  const request = await response.json();
  // needsSignIn describes the browser at the moment the application asked,
  // so it stays true after a sign-in. Only a refused read sends the person
  // back to the sign-in page, or the two pages bounce forever.
  document.getElementById("title").textContent = request.clientName + " asks for access";
  document.getElementById("summary").textContent =
    "It will act on your sandboxes with the access you allow here.";
  const list = document.getElementById("scopes");
  for (const scope of request.scopes || []) {
    const item = document.createElement("li");
    item.textContent = scope;
    list.appendChild(item);
  }
  document.getElementById("buttons").hidden = false;
}

async function decide(approve) {
  error.textContent = "";
  const response = await fetch(prefix + "/oauth2/decide", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ requestId, approve }),
  });
  if (!response.ok) {
    const body = await response.json().catch(() => ({}));
    error.textContent = body?.error?.message || "The decision failed. Start again from the application.";
    return;
  }
  const result = await response.json();
  window.location.href = result.redirectTo;
}

document.getElementById("approve").addEventListener("click", () => decide(true));
document.getElementById("deny").addEventListener("click", () => decide(false));
load();
</script>
</html>`))

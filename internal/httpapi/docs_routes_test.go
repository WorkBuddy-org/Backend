package httpapi_test

import (
	"net/http"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/workbuddy/backend/internal/auth"
	"github.com/workbuddy/backend/internal/call"
	"github.com/workbuddy/backend/internal/chat"
	"github.com/workbuddy/backend/internal/httpapi"
	"github.com/workbuddy/backend/internal/invite"
	"github.com/workbuddy/backend/internal/profile"
	"github.com/workbuddy/backend/internal/room"
	"github.com/workbuddy/backend/internal/task"
	"github.com/workbuddy/backend/internal/ws"
)

// docsRouteHeading matches an endpoint heading in docs/api.md, e.g.
// ### `POST /rooms/{id}/join`
var docsRouteHeading = regexp.MustCompile("(?m)^### `(GET|POST|PUT|PATCH|DELETE) (/[^`]*)`$")

// TestDocumentedRoutesMatchRouter keeps docs/api.md honest: every endpoint
// heading must be a registered route, and every registered route must have a
// heading. Handlers are built without services; only registration runs.
func TestDocumentedRoutesMatchRouter(t *testing.T) {
	router := httpapi.NewRouter(httpapi.RouterDeps{
		AuthHandler:    auth.NewHandler(nil),
		AuthMW:         auth.NewMiddleware(nil),
		RoomHandler:    room.NewHandler(nil),
		InviteHandler:  invite.NewHandler(nil),
		TaskHandler:    task.NewHandler(nil),
		ProfileHandler: profile.NewHandler(nil),
		ChatHandler:    chat.NewHandler(nil),
		WSHandler:      ws.NewHandler(ws.HandlerDeps{}),
		CallHandler:    call.NewHandler(nil),
	})

	var registered []string
	err := chi.Walk(router.(chi.Routes), func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		registered = append(registered, method+" "+route)
		return nil
	})
	if err != nil {
		t.Fatalf("walk router: %v", err)
	}

	raw, err := os.ReadFile("../../docs/api.md")
	if err != nil {
		t.Fatalf("read docs: %v", err)
	}
	var documented []string
	for _, m := range docsRouteHeading.FindAllStringSubmatch(strings.ReplaceAll(string(raw), "\r\n", "\n"), -1) {
		documented = append(documented, m[1]+" "+m[2])
	}

	for _, d := range documented {
		if !slices.Contains(registered, d) {
			t.Errorf("docs/api.md documents %q, which the router does not register", d)
		}
	}
	for _, r := range registered {
		if !slices.Contains(documented, r) {
			t.Errorf("router registers %q, which docs/api.md does not document", r)
		}
	}
	if len(documented) == 0 {
		t.Fatal("no endpoint headings found in docs/api.md")
	}
}

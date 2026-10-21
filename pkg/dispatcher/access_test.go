package dispatcher

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"log/slog"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/Zuful/rikugan/pkg/provider"
)

// declaredTools finds every mcp.NewTool call in the module's non-test
// sources and returns the tool names, by file. A name that is not a
// string literal is reported as such: classification needs it known.
func declaredTools(t *testing.T) map[string]string {
	t.Helper()
	root := filepath.Join("..", "..")
	found := map[string]string{}
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if e.IsDir() {
			switch e.Name() {
			case ".git", "vendor", "testdata", "node_modules":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "NewTool" {
				return true
			}
			if x, ok := sel.X.(*ast.Ident); !ok || x.Name != "mcp" || len(call.Args) == 0 {
				return true
			}
			lit, ok := call.Args[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				t.Errorf("%s: mcp.NewTool with a name that is not a string literal: it cannot be classified", fset.Position(call.Pos()))
				return true
			}
			name, _ := strconv.Unquote(lit.Value)
			found[name] = fset.Position(call.Pos()).String()
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return found
}

// Every tool is classified, so that read-only mode knows what to
// withhold; and the classification names no tool that is gone.
func TestEveryToolIsClassified(t *testing.T) {
	declared := declaredTools(t)
	if len(declared) < 20 {
		t.Fatalf("found only %d tools: the source walk is broken", len(declared))
	}
	for name, at := range declared {
		if _, ok := AccessOf(name); !ok {
			t.Errorf("%s: tool %q is not classified as read or write in toolAccess", at, name)
		}
	}
	for name := range toolAccess {
		if _, ok := declared[name]; !ok {
			t.Errorf("toolAccess classifies %q, which no source declares", name)
		}
	}
}

// The tools that change something, named: a new one must be a choice.
func TestWriteToolsAreKnown(t *testing.T) {
	var writes []string
	for name, a := range toolAccess {
		if a == Write {
			writes = append(writes, name)
		}
	}
	if len(writes) != 1 || writes[0] != "restart_service" {
		t.Errorf("write tools = %v, want [restart_service]", writes)
	}
}

func tools(names ...string) *stubProvider {
	p := &stubProvider{name: "p"}
	for _, n := range names {
		p.tools = append(p.tools, provider.ToolDefinition{Tool: mcp.NewTool(n), Handler: noopHandler})
	}
	return p
}

func TestReadOnlyWithholdsWriteAndUnclassifiedTools(t *testing.T) {
	srv := server.NewMCPServer("test", "0.0.0")
	d := New(srv, discard(), WithReadOnly(true))
	if err := d.Register(tools("get_logs", "restart_service", "brand_new_tool")); err != nil {
		t.Fatal(err)
	}
	got := srv.ListTools()
	if len(got) != 1 || got["get_logs"] == nil {
		t.Errorf("registered %v, want only get_logs", keys(got))
	}
}

func TestDefaultModeRegistersEverythingWithAnnotations(t *testing.T) {
	srv := server.NewMCPServer("test", "0.0.0")
	d := New(srv, discard())
	if err := d.Register(tools("get_logs", "restart_service", "brand_new_tool")); err != nil {
		t.Fatal(err)
	}
	for name, readOnly := range map[string]bool{"get_logs": true, "restart_service": false, "brand_new_tool": false} {
		st := srv.GetTool(name)
		if st == nil {
			t.Errorf("%s not registered", name)
			continue
		}
		a := st.Tool.Annotations
		if a.ReadOnlyHint == nil || *a.ReadOnlyHint != readOnly || a.DestructiveHint == nil || *a.DestructiveHint == readOnly {
			t.Errorf("%s annotations = readOnly %v destructive %v, want readOnly %v", name, a.ReadOnlyHint, a.DestructiveHint, readOnly)
		}
	}
}

func keys(m map[string]*server.ServerTool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

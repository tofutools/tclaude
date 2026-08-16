package agentd

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Every production permission constant is part of the registry. Parsing the
// package keeps the test future-proof: adding a new PermFoo constant without a
// catalog entry fails without anyone updating a second hand-maintained list.
func TestPermissionRegistryContainsEveryPermissionConstant(t *testing.T) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	require.NoError(t, err)
	for _, entry := range entries {
		filename := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(filename, ".go") || strings.HasSuffix(filename, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filename, nil, 0)
		require.NoError(t, err, filename)
		ast.Inspect(file, func(node ast.Node) bool {
			decl, ok := node.(*ast.GenDecl)
			if !ok || decl.Tok != token.CONST {
				return true
			}
			for _, spec := range decl.Specs {
				valueSpec, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, name := range valueSpec.Names {
					if !strings.HasPrefix(name.Name, "Perm") || i >= len(valueSpec.Values) {
						continue
					}
					literal, ok := valueSpec.Values[i].(*ast.BasicLit)
					if !ok || literal.Kind != token.STRING {
						continue
					}
					slug, err := strconv.Unquote(literal.Value)
					require.NoError(t, err)
					assert.Truef(t, IsKnownPermSlug(slug),
						"%s (%s in %s) is missing from permissionRegistry", slug, name.Name, filename)
				}
			}
			return false
		})
	}
}

func TestRequirePermissionRefusesUnregisteredSlugBeforeHumanBypass(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/test", nil)
	req = AsHumanPeer(req)
	rec := httptest.NewRecorder()
	_, ok := requirePermission(rec, req, "groups.teleport")
	assert.False(t, ok)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Contains(t, rec.Body.String(), "unregistered_permission")
}

func TestGroupsAdminCoversRegisteredGroupAdministrationAndDedicatedDenyWins(t *testing.T) {
	src := permSources{
		resolvable: true,
		sudo:       map[string]sudoPermSource{},
		override: map[string]overridePermSource{
			PermGroupsAdmin: {Effect: db.PermEffectGrant},
		},
		group:     map[string][]string{},
		groupRows: map[string][]db.AgentGroupPermission{},
	}
	for _, entry := range permissionRegistry {
		if !IsGroupsAdminImpliedSlug(entry.Slug) {
			continue
		}
		t.Run(entry.Slug, func(t *testing.T) {
			got := resolveEffectivePermissionVerdictFrom(src, entry.Slug, false, false)
			assert.Equal(t, permAllow, got.Resolution)

			denied := src
			denied.override = make(map[string]overridePermSource, len(src.override)+1)
			for slug, override := range src.override {
				denied.override[slug] = override
			}
			denied.override[entry.Slug] = overridePermSource{Effect: db.PermEffectDeny}
			got = resolveEffectivePermissionVerdictFrom(denied, entry.Slug, false, false)
			assert.Equal(t, permDeny, got.Resolution)
		})
	}
}

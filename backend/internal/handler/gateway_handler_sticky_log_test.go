package handler

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestGatewayHandlerDoesNotLogRawStickyIdentifiers(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "gateway_handler.go", nil, 0)
	require.NoError(t, err)

	var violations []string
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != "String" {
			return true
		}
		pkg, ok := selector.X.(*ast.Ident)
		if !ok || pkg.Name != "zap" {
			return true
		}
		field, ok := call.Args[0].(*ast.BasicLit)
		if !ok || field.Kind != token.STRING {
			return true
		}
		name, err := strconv.Unquote(field.Value)
		if err != nil || (name != "session_hash" && name != "session_key") {
			return true
		}
		violations = append(violations, fmt.Sprintf("%s uses zap.String(%q)", fset.Position(call.Pos()), name))
		return true
	})

	require.Empty(t, violations, "sticky identifiers must be logged only as presence signals")
}

func TestLogStickySessionHashGeneratedRedactsSensitiveInputs(t *testing.T) {
	const (
		sessionHash    = "session-token-sk-ant-test-secret"
		metadataUserID = `user-private_account_request-body-feature_session_bearer-token-secret`
	)
	core, logs := observer.New(zap.InfoLevel)
	log := zap.New(core).With(zap.String("request_id", "safe-request-correlation"))

	logStickySessionHashGenerated(log, sessionHash, metadataUserID)

	entries := logs.FilterMessage("sticky.session_hash_generated").All()
	require.Len(t, entries, 1)
	fields := entries[0].ContextMap()
	require.Equal(t, "safe-request-correlation", fields["request_id"])
	require.Equal(t, true, fields["session_hash_present"])
	require.Equal(t, true, fields["metadata_user_id_present"])
	require.Len(t, fields, 3)
	require.NotContains(t, fields, "session_hash")
	require.NotContains(t, fields, "metadata_user_id_raw")

	serializedFields := fmt.Sprint(fields)
	for _, secret := range []string{
		sessionHash,
		metadataUserID,
		"request-body-feature",
		"bearer-token-secret",
		"sk-ant-test-secret",
	} {
		require.False(t, strings.Contains(serializedFields, secret), "log fields leaked %q", secret)
	}
}

func TestLogStickySessionHashGeneratedRecordsOnlyAbsence(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)

	logStickySessionHashGenerated(zap.New(core), " \t", "\n")

	entries := logs.FilterMessage("sticky.session_hash_generated").All()
	require.Len(t, entries, 1)
	require.Equal(t, map[string]any{
		"session_hash_present":     false,
		"metadata_user_id_present": false,
	}, entries[0].ContextMap())
}

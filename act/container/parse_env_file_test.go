// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package container

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/text/encoding/unicode"
)

func TestParseEnvFile(t *testing.T) {
	t.Parallel()
	hostEnv := &HostEnvironment{Path: t.TempDir()}
	largeValue := strings.Repeat("x", 2*1024*1024)
	encodeUTF16 := func(endianness unicode.Endianness) string {
		content, err := unicode.UTF16(endianness, unicode.UseBOM).NewEncoder().Bytes([]byte("FOO=bar\r\nMULTI<<EOF\r\nline1\r\nEOF\r\n"))
		require.NoError(t, err)
		return string(content)
	}
	for _, test := range []struct {
		name, content string
		want          map[string]string
	}{
		{"single line", "FOO=bar\nBAZ=qux\n", map[string]string{"FOO": "bar", "BAZ": "qux"}},
		{"repeated names", "FOO=first\nfoo=middle\nFOO=last\n", map[string]string{"FOO": "last", "foo": "middle"}},
		{"single line CRLF and final bare CR", "FOO=bar\r\nBAZ=qux\r", map[string]string{"FOO": "bar\r", "BAZ": "qux\r"}},
		{"single line without final newline", "FOO=bar", map[string]string{"FOO": "bar"}},
		{"blank lines", "\nFOO=bar\n\n   \nBAZ=qux\n\n", map[string]string{"FOO": "bar", "BAZ": "qux"}},
		{"multiline", "FOO<<EOF\nline1\nline2\nEOF\n", map[string]string{"FOO": "line1\nline2"}},
		{"multiline without final newline", "FOO<<EOF\nline1\r\nline2\r\nEOF", map[string]string{"FOO": "line1\r\nline2\r"}},
		{"empty multiline without final newline", "FOO<<EOF\nEOF", map[string]string{"FOO": ""}},
		{"multiline blank lines", "FOO<<EOF\nline1\n\nline2\nEOF\n", map[string]string{"FOO": "line1\n\nline2"}},
		{"multiline leading blank line", "FOO<<EOF\n\nline2\nEOF\n", map[string]string{"FOO": "\nline2"}},
		{"multiline CRLF", "FOO<<EOF\r\nline1\r\nline2\r\nEOF\r\n", map[string]string{"FOO": "line1\r\nline2\r"}},
		{"multiline mixed line endings", "FOO<<EOF\nline1\r\nline2\n\r\nEOF\n", map[string]string{"FOO": "line1\r\nline2\n\r"}},
		{"large value within limit", "FOO<<EOF\n" + largeValue + "\nEOF\n", map[string]string{"FOO": largeValue}},
		{"UTF8 BOM", "\xef\xbb\xbfFOO=bar\n", map[string]string{"FOO": "bar"}},
		{"UTF16 little endian", encodeUTF16(unicode.LittleEndian), map[string]string{"FOO": "bar\r", "MULTI": "line1\r"}},
		{"UTF16 big endian", encodeUTF16(unicode.BigEndian), map[string]string{"FOO": "bar\r", "MULTI": "line1\r"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			envPath := filepath.Join(hostEnv.Path, test.name)
			require.NoError(t, os.WriteFile(envPath, []byte(test.content), 0o600))
			env := map[string]string{}
			require.NoError(t, parseEnvFile(hostEnv, envPath, &env, false)(t.Context()))
			assert.Equal(t, test.want, env)
		})
	}
	t.Run("ordered entries preserve repeated names", func(t *testing.T) {
		envPath := filepath.Join(hostEnv.Path, "ordered")
		require.NoError(t, os.WriteFile(envPath, []byte("FOO=first\r\nfoo<<EOF\r\nmiddle\r\nEOF\r\nFOO=last\r\n"), 0o600))
		var entries [][2]string
		require.NoError(t, ParseEnvFile(hostEnv, envPath, func(name, value string) {
			entries = append(entries, [2]string{name, value})
		}, true)(t.Context()))
		assert.Equal(t, [][2]string{
			{"FOO", "first"}, {"foo", "middle"}, {"FOO", "last"},
		}, entries)
	})
	t.Run("Windows line endings", func(t *testing.T) {
		envPath := filepath.Join(hostEnv.Path, "windows")
		require.NoError(t, os.WriteFile(envPath, []byte("FOO=bar\r\nMULTI<<EOF\r\nline1\r\nline2\nEOF\r\n"), 0o600))
		env := map[string]string{}
		require.NoError(t, parseEnvFile(hostEnv, envPath, &env, true)(t.Context()))
		assert.Equal(t, map[string]string{"FOO": "bar", "MULTI": "line1\r\nline2"}, env)
	})
}

func TestParseEnvFileLineExceedsBufferReportsScannerError(t *testing.T) {
	t.Parallel()
	hostEnv := &HostEnvironment{Path: t.TempDir()}
	envPath := filepath.Join(hostEnv.Path, "envfile")
	const header, footer = "FOO<<EOF\n", "\nEOF\n"
	content := bytes.Repeat([]byte("x"), len(header)+17*1024*1024+len(footer))
	copy(content, header)
	copy(content[len(content)-len(footer):], footer)
	require.NoError(t, os.WriteFile(envPath, content, 0o600))

	env := map[string]string{}
	err := parseEnvFile(hostEnv, envPath, &env, false)(t.Context())
	require.ErrorIs(t, err, bufio.ErrTooLong)
	assert.Contains(t, err.Error(), "reading env file")
}

func TestParseEnvFileInvalidHeredoc(t *testing.T) {
	hostEnv := &HostEnvironment{Path: t.TempDir()}
	for name, content := range map[string]string{
		"missing delimiter":              "FOO<<EOF\nline1\nline2\n",
		"empty name":                     "<<EOF\nline1\nEOF\n",
		"empty delimiter":                "FOO<<\nline1\n\n",
		"delimiter with carriage return": "FOO<<EOF\nline1\nEOF\r\n",
	} {
		t.Run(name, func(t *testing.T) {
			envPath := filepath.Join(hostEnv.Path, name)
			require.NoError(t, os.WriteFile(envPath, []byte("APPLIED=before-error\n"+content), 0o600))

			env := map[string]string{}
			err := parseEnvFile(hostEnv, envPath, &env, false)(t.Context())
			require.Error(t, err)
			assert.Contains(t, err.Error(), "delimiter")
			assert.Equal(t, map[string]string{"APPLIED": "before-error"}, env)
		})
	}
}

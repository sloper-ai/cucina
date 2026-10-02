// SPDX-License-Identifier: FSL-1.1-ALv2

package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Guards the chart's hook and init-container command lines (ADR 0406): every
// subcommand the chart runs exists and rejects stray arguments.
func TestChartSubcommandsExist(t *testing.T) {
	root := newRoot()
	for _, args := range [][]string{
		{"crds", "apply"},
		{"bootstrap"},
		{"uninstall-prep"},
		{"controller"},
		{"sts"},
		{"canary", "cache"},
	} {
		cmd, rest, err := root.Find(args)
		require.NoError(t, err, args)
		assert.Empty(t, rest, args)
		assert.Equal(t, args[len(args)-1], cmd.Name(), args)
		assert.Error(t, cmd.ValidateArgs([]string{"stray"}), "%v must take no positional arguments", args)
	}
	apply, _, err := root.Find([]string{"crds", "apply"})
	require.NoError(t, err)
	assert.NotNil(t, apply.Flags().Lookup("timeout"))
}

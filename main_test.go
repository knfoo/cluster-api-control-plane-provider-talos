// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package main

import (
	"testing"

	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/cluster-api/feature"
)

func TestInitFlagsAddsFeatureGates(t *testing.T) {
	defer func() {
		if err := feature.MutableGates.Set("InPlaceUpdates=false"); err != nil {
			t.Fatalf("restoring the InPlaceUpdates gate: %v", err)
		}
	}()

	fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
	InitFlags(fs)

	require.NotNil(t, fs.Lookup("feature-gates"), "--feature-gates must be registered so the InPlaceUpdates gate can be enabled")

	assert.NoError(t, fs.Parse([]string{"--feature-gates=InPlaceUpdates=true"}))
	assert.True(t, feature.Gates.Enabled(feature.InPlaceUpdates), "the InPlaceUpdates gate must be settable through --feature-gates")
}

package devserver

import (
	"testing"

	"go.temporal.io/server/common/dynamicconfig"
)

func TestStartDevServerFeatureOverridesMatchServerDefaults(t *testing.T) {
	dc := dynamicconfig.NewNoopCollection()
	for _, override := range startDevServerFeatureOverrides {
		key := override.setting.Key().String()
		t.Run(key, func(t *testing.T) {
			if serverDefault := override.setting.Get(dc)("default"); serverDefault == override.enabled {
				t.Fatalf("%q now defaults to %t; remove its feature override", key, override.enabled)
			}
		})
	}
}

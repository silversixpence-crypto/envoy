package node

import (
	"testing"

	"github.com/trisacrypto/envoy/pkg/config"

	"github.com/stretchr/testify/require"
)

// A TRP-only node must not attempt to start the directory sync service since there is
// no directory client on a disabled TRISA network to sync from.
func TestDisableDirectorySync(t *testing.T) {
	t.Run("TRISADisabled", func(t *testing.T) {
		conf := config.Config{
			Node:          config.TRISAConfig{Enabled: false},
			DirectorySync: config.DirectorySyncConfig{Enabled: true},
		}

		conf = disableDirectorySync(conf)
		require.False(t, conf.DirectorySync.Enabled, "directory sync should be forced off when the trisa node is disabled")
	})

	t.Run("TRISAEnabled", func(t *testing.T) {
		conf := config.Config{
			Node:          config.TRISAConfig{Enabled: true},
			DirectorySync: config.DirectorySyncConfig{Enabled: true},
		}

		conf = disableDirectorySync(conf)
		require.True(t, conf.DirectorySync.Enabled, "directory sync should be left alone when the trisa node is enabled")
	})

	t.Run("AlreadyDisabled", func(t *testing.T) {
		conf := config.Config{
			Node:          config.TRISAConfig{Enabled: false},
			DirectorySync: config.DirectorySyncConfig{Enabled: false},
		}

		conf = disableDirectorySync(conf)
		require.False(t, conf.DirectorySync.Enabled, "directory sync should remain off")
	})
}

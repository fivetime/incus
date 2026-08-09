//go:build linux && cgo && !agent

package db

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestIDMapUsageQueriesUsePartialExpressionIndexes(t *testing.T) {
	database, cleanup := NewTestCluster(t)
	defer cleanup()

	tests := []struct {
		name      string
		query     string
		arguments []any
		indexName string
	}{
		{
			name: "instance owner",
			query: `SELECT instance_id FROM instances_config
WHERE key = 'user.openstack.uuid'
  AND replace(replace(replace(replace(lower(value), '-', ''), '{', ''), '}', ''), 'urn:uuid:', '') = ?`,
			arguments: []any{"aaaaaaaaaaaa4aaa8aaaaaaaaaaaaaaa"},
			indexName: "instances_config_idmap_owner_usage_idx",
		},
		{
			name: "instance range",
			query: `SELECT instance_id FROM instances_config
WHERE key = 'security.idmap.base'
  AND CAST(value AS INTEGER) >= ? AND CAST(value AS INTEGER) < ?`,
			arguments: []any{int64(934465), int64(1065536)},
			indexName: "instances_config_idmap_range_usage_idx",
		},
		{
			name: "instance volatile range",
			query: `SELECT instance_id FROM instances_config
WHERE key = 'volatile.idmap.base'
  AND CAST(value AS INTEGER) >= ? AND CAST(value AS INTEGER) < ?`,
			arguments: []any{int64(934465), int64(1065536)},
			indexName: "instances_config_idmap_volatile_range_usage_idx",
		},
		{
			name: "instance nondefault size",
			query: `SELECT instance_id FROM instances_config
WHERE key = 'security.idmap.size'
  AND value NOT IN ('', 'auto', '65536')`,
			indexName: "instances_config_idmap_nondefault_size_usage_idx",
		},
		{
			name: "instance malformed",
			query: `SELECT instances_config.instance_id, key FROM instances_config INDEXED BY instances_config_idmap_malformed_usage_idx
JOIN instances ON instances.id = instances_config.instance_id
JOIN projects ON projects.id = instances.project_id
WHERE (
      (key IN ('security.idmap.base', 'volatile.idmap.base')
       AND value != ''
       AND ((CASE WHEN substr(value, 1, 1) = '+' THEN substr(value, 2) ELSE value END) = ''
            OR (CASE WHEN substr(value, 1, 1) = '+' THEN substr(value, 2) ELSE value END) GLOB '*[^0-9]*'
            OR CAST(value AS INTEGER) < 0
            OR CAST(value AS INTEGER) > 4294967295))
      OR
      (key = 'security.idmap.size'
       AND value NOT IN ('', 'auto')
       AND ((CASE WHEN substr(value, 1, 1) = '+' THEN substr(value, 2) ELSE value END) = ''
            OR (CASE WHEN substr(value, 1, 1) = '+' THEN substr(value, 2) ELSE value END) GLOB '*[^0-9]*'
            OR CAST(value AS INTEGER) <= 0
            OR CAST(value AS INTEGER) > 4294967295))
      ) LIMIT 1`,
			indexName: "instances_config_idmap_malformed_usage_idx",
		},
		{
			name: "profile owner",
			query: `SELECT profile_id FROM profiles_config
WHERE key = 'user.openstack.uuid'
  AND replace(replace(replace(replace(lower(value), '-', ''), '{', ''), '}', ''), 'urn:uuid:', '') = ?`,
			arguments: []any{"aaaaaaaaaaaa4aaa8aaaaaaaaaaaaaaa"},
			indexName: "profiles_config_idmap_owner_usage_idx",
		},
		{
			name: "profile range",
			query: `SELECT profile_id FROM profiles_config
WHERE key = 'security.idmap.base'
  AND CAST(value AS INTEGER) >= ? AND CAST(value AS INTEGER) < ?`,
			arguments: []any{int64(934465), int64(1065536)},
			indexName: "profiles_config_idmap_range_usage_idx",
		},
		{
			name: "profile nondefault size",
			query: `SELECT profile_id FROM profiles_config
WHERE key = 'security.idmap.size'
  AND value NOT IN ('', 'auto', '65536')`,
			indexName: "profiles_config_idmap_nondefault_size_usage_idx",
		},
		{
			name: "profile malformed",
			query: `SELECT profiles_config.profile_id, key FROM profiles_config INDEXED BY profiles_config_idmap_malformed_usage_idx
JOIN profiles ON profiles.id = profiles_config.profile_id
JOIN projects ON projects.id = profiles.project_id
WHERE (
      (key = 'security.idmap.base'
       AND value != ''
       AND ((CASE WHEN substr(value, 1, 1) = '+' THEN substr(value, 2) ELSE value END) = ''
            OR (CASE WHEN substr(value, 1, 1) = '+' THEN substr(value, 2) ELSE value END) GLOB '*[^0-9]*'
            OR CAST(value AS INTEGER) < 0
            OR CAST(value AS INTEGER) > 4294967295))
      OR
      (key = 'security.idmap.size'
       AND value NOT IN ('', 'auto')
       AND ((CASE WHEN substr(value, 1, 1) = '+' THEN substr(value, 2) ELSE value END) = ''
            OR (CASE WHEN substr(value, 1, 1) = '+' THEN substr(value, 2) ELSE value END) GLOB '*[^0-9]*'
            OR CAST(value AS INTEGER) <= 0
            OR CAST(value AS INTEGER) > 4294967295))
      ) LIMIT 1`,
			indexName: "profiles_config_idmap_malformed_usage_idx",
		},
	}

	err := database.Transaction(context.Background(), func(ctx context.Context, tx *ClusterTx) error {
		for _, tt := range tests {
			rows, err := tx.Tx().QueryContext(
				ctx, "EXPLAIN QUERY PLAN "+tt.query, tt.arguments...)
			if err != nil {
				return fmt.Errorf("Explain %s query: %w", tt.name, err)
			}

			found := false
			for rows.Next() {
				var id int
				var parent int
				var unused int
				var detail string
				err = rows.Scan(&id, &parent, &unused, &detail)
				if err != nil {
					_ = rows.Close()
					return err
				}

				found = found || strings.Contains(detail, tt.indexName)
			}

			err = rows.Close()
			if err != nil {
				return err
			}

			if !found {
				return fmt.Errorf("%s query did not use %s", tt.name, tt.indexName)
			}
		}

		return nil
	})
	require.NoError(t, err)
}

func TestGetIDMapUsageExpandedConfig(t *testing.T) {
	database, cleanup := NewTestCluster(t)
	defer cleanup()

	const owner = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	const otherOwner = "22222222-2222-4222-8222-222222222222"
	const base = "1000000"
	const size = "65536"

	err := database.Transaction(context.Background(), func(ctx context.Context, tx *ClusterTx) error {
		profileOwner := insertIDMapUsageProfile(t, tx, "owner-profile", map[string]string{
			"user.openstack.uuid": "AAAAAAAA-AAAA-4AAA-8AAA-AAAAAAAAAAAA",
		})
		profileRange := insertIDMapUsageProfile(t, tx, "range-profile", map[string]string{
			"security.idmap.base": base,
			"security.idmap.size": size,
		})
		profileOverride := insertIDMapUsageProfile(t, tx, "override-profile", map[string]string{
			"user.openstack.uuid": otherOwner,
			"security.idmap.size": "131072",
		})
		profileBaseOverrideAway := insertIDMapUsageProfile(t, tx, "base-override-away-profile", map[string]string{
			"security.idmap.base": "3000000",
		})
		profileCustomSizeOverlap := insertIDMapUsageProfile(t, tx, "custom-size-overlap-profile", map[string]string{
			"security.idmap.size": "200000",
		})

		localOwner := insertIDMapUsageInstance(t, tx, "local-owner", map[string]string{
			"user.openstack.uuid": "{AAAAAAAA-AAAA-4AAA-8AAA-AAAAAAAAAAAA}",
		})
		_ = localOwner
		inheritedOwner := insertIDMapUsageInstance(t, tx, "inherited-owner", nil)
		insertIDMapUsageInstanceProfile(t, tx, inheritedOwner, profileOwner, 1)

		overriddenOwner := insertIDMapUsageInstance(t, tx, "overridden-owner", nil)
		insertIDMapUsageInstanceProfile(t, tx, overriddenOwner, profileOwner, 1)
		insertIDMapUsageInstanceProfile(t, tx, overriddenOwner, profileOverride, 2)

		inheritedRange := insertIDMapUsageInstance(t, tx, "inherited-range", nil)
		insertIDMapUsageInstanceProfile(t, tx, inheritedRange, profileRange, 1)

		overriddenRange := insertIDMapUsageInstance(t, tx, "overridden-range", nil)
		insertIDMapUsageInstanceProfile(t, tx, overriddenRange, profileRange, 1)
		insertIDMapUsageInstanceProfile(t, tx, overriddenRange, profileOverride, 2)
		profileBaseOverrideAwayInstance := insertIDMapUsageInstance(t, tx, "profile-base-override-away", nil)
		insertIDMapUsageInstanceProfile(t, tx, profileBaseOverrideAwayInstance, profileRange, 1)
		insertIDMapUsageInstanceProfile(t, tx, profileBaseOverrideAwayInstance, profileBaseOverrideAway, 2)
		emptyLocalBase := insertIDMapUsageInstance(t, tx, "empty-local-base", map[string]string{
			"security.idmap.base": "",
		})
		insertIDMapUsageInstanceProfile(t, tx, emptyLocalBase, profileRange, 1)
		localBaseOverrideAway := insertIDMapUsageInstance(t, tx, "local-base-override-away", map[string]string{
			"security.idmap.base": "3000000",
		})
		insertIDMapUsageInstanceProfile(t, tx, localBaseOverrideAway, profileRange, 1)
		localSizeOverrideAway := insertIDMapUsageInstance(t, tx, "local-size-override-away", map[string]string{
			"security.idmap.base": "900000",
			"security.idmap.size": "1",
		})
		insertIDMapUsageInstanceProfile(t, tx, localSizeOverrideAway, profileCustomSizeOverlap, 1)
		emptyLocalSizeOverrideAway := insertIDMapUsageInstance(t, tx, "empty-local-size-override-away", map[string]string{
			"security.idmap.base": "900000",
			"security.idmap.size": "",
		})
		insertIDMapUsageInstanceProfile(t, tx, emptyLocalSizeOverrideAway, profileCustomSizeOverlap, 1)
		profileSizeOverrideOverlap := insertIDMapUsageInstance(t, tx, "profile-size-override-overlap", map[string]string{
			"security.idmap.base": "900000",
		})
		insertIDMapUsageInstanceProfile(t, tx, profileSizeOverrideOverlap, profileRange, 1)
		insertIDMapUsageInstanceProfile(t, tx, profileSizeOverrideOverlap, profileCustomSizeOverlap, 2)

		localRange := insertIDMapUsageInstance(t, tx, "local-range", map[string]string{
			"security.idmap.base": "01000000",
			"security.idmap.size": "065536",
		})
		insertIDMapUsageInstanceProfile(t, tx, localRange, profileOverride, 1)

		leadingRangeProfile := insertIDMapUsageProfile(
			t, tx, "leading-range-profile", map[string]string{
				"security.idmap.base": "01000000",
				"security.idmap.size": "065536",
			})
		leadingInheritedRange := insertIDMapUsageInstance(
			t, tx, "leading-inherited-range", nil)
		insertIDMapUsageInstanceProfile(
			t, tx, leadingInheritedRange, leadingRangeProfile, 1)

		insertIDMapUsageInstance(t, tx, "default-size", map[string]string{
			"security.idmap.base": "950000",
		})
		insertIDMapUsageInstance(t, tx, "empty-size", map[string]string{
			"security.idmap.base": "1065535",
			"security.idmap.size": "",
		})
		profileDefaultSize := insertIDMapUsageProfile(
			t, tx, "profile-default-size", map[string]string{
				"security.idmap.base": "999999",
			})
		profileDefaultSizeInstance := insertIDMapUsageInstance(
			t, tx, "profile-default-size-instance", nil)
		insertIDMapUsageInstanceProfile(
			t, tx, profileDefaultSizeInstance, profileDefaultSize, 1)
		insertIDMapUsageInstance(t, tx, "containing-range", map[string]string{
			"security.idmap.base": "900000",
			"security.idmap.size": "200000",
		})
		insertIDMapUsageInstance(t, tx, "plus-range", map[string]string{
			"security.idmap.base": "+1000000",
			"security.idmap.size": "+65536",
		})
		insertIDMapUsageInstance(t, tx, "high-valid-default", map[string]string{
			"security.idmap.base": "4294901760",
		})
		insertIDMapUsageInstance(t, tx, "high-valid-single", map[string]string{
			"security.idmap.base": "4294967295",
			"security.idmap.size": "1",
		})
		insertIDMapUsageInstance(t, tx, "volatile-overlap", map[string]string{
			"security.idmap.base": "3000000",
			"volatile.idmap.base": "999999",
			"security.idmap.size": size,
		})
		insertIDMapUsageInstance(t, tx, "empty-volatile-fallback", map[string]string{
			"security.idmap.base": base,
			"volatile.idmap.base": "",
			"security.idmap.size": size,
		})
		insertIDMapUsageInstance(t, tx, "volatile-override-away", map[string]string{
			"security.idmap.base": base,
			"volatile.idmap.base": "3000000",
			"security.idmap.size": size,
		})
		insertIDMapUsageInstance(t, tx, "adjacent-left", map[string]string{
			"security.idmap.base": "934464",
			"security.idmap.size": size,
		})
		insertIDMapUsageInstance(t, tx, "adjacent-right", map[string]string{
			"security.idmap.base": "1065536",
			"security.idmap.size": size,
		})
		autoSizeProfile := insertIDMapUsageProfile(
			t, tx, "auto-size-profile", map[string]string{
				"security.idmap.base": "1065535",
				"security.idmap.size": "auto",
			})
		autoSizeInstance := insertIDMapUsageInstance(
			t, tx, "auto-size-instance", nil)
		insertIDMapUsageInstanceProfile(
			t, tx, autoSizeInstance, autoSizeProfile, 1)
		return nil
	})
	require.NoError(t, err)

	var resources []IDMapUsageResource
	err = database.Transaction(context.Background(), func(ctx context.Context, tx *ClusterTx) error {
		var err error
		resources, err = tx.GetIDMapUsage(ctx, owner, 1000000, 65536)
		return err
	})
	require.NoError(t, err)
	require.Equal(t, []IDMapUsageResource{
		{Type: "instance", Project: "default", Name: "auto-size-instance"},
		{Type: "instance", Project: "default", Name: "containing-range"},
		{Type: "instance", Project: "default", Name: "default-size"},
		{Type: "instance", Project: "default", Name: "empty-size"},
		{Type: "instance", Project: "default", Name: "empty-volatile-fallback"},
		{Type: "instance", Project: "default", Name: "inherited-owner"},
		{Type: "instance", Project: "default", Name: "inherited-range"},
		{Type: "instance", Project: "default", Name: "leading-inherited-range"},
		{Type: "instance", Project: "default", Name: "local-owner"},
		{Type: "instance", Project: "default", Name: "local-range"},
		{Type: "instance", Project: "default", Name: "overridden-range"},
		{Type: "instance", Project: "default", Name: "plus-range"},
		{Type: "instance", Project: "default", Name: "profile-default-size-instance"},
		{Type: "instance", Project: "default", Name: "profile-size-override-overlap"},
		{Type: "instance", Project: "default", Name: "volatile-overlap"},
		{Type: "profile", Project: "default", Name: "auto-size-profile"},
		{Type: "profile", Project: "default", Name: "leading-range-profile"},
		{Type: "profile", Project: "default", Name: "owner-profile"},
		{Type: "profile", Project: "default", Name: "profile-default-size"},
		{Type: "profile", Project: "default", Name: "range-profile"},
	}, resources)

	err = database.Transaction(context.Background(), func(ctx context.Context, tx *ClusterTx) error {
		var err error
		resources, err = tx.GetIDMapUsage(ctx, "33333333-3333-4333-8333-333333333333", 4294967295, 1)
		return err
	})
	require.NoError(t, err)
	require.Equal(t, []IDMapUsageResource{
		{Type: "instance", Project: "default", Name: "high-valid-default"},
		{Type: "instance", Project: "default", Name: "high-valid-single"},
	}, resources)

	err = database.Transaction(context.Background(), func(ctx context.Context, tx *ClusterTx) error {
		var err error
		resources, err = tx.GetIDMapUsage(ctx, "33333333-3333-4333-8333-333333333333", 0, 1)
		return err
	})
	require.NoError(t, err)
	require.Empty(t, resources)
}

func TestGetIDMapUsageRejectsInvalidQueryRange(t *testing.T) {
	const owner = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	tests := []struct {
		name      string
		base      uint64
		size      uint64
		errorText string
	}{
		{
			name:      "zero size",
			base:      1000000,
			errorText: "size must be greater than zero",
		},
		{
			name:      "base exceeds uint32",
			base:      1 << 32,
			size:      1,
			errorText: "base 4294967296 exceeds the uint32 maximum",
		},
		{
			name:      "size exceeds uint32",
			base:      0,
			size:      1 << 32,
			errorText: "size 4294967296 exceeds the uint32 maximum",
		},
		{
			name:      "range overflows uint32 address space",
			base:      1<<32 - 1,
			size:      2,
			errorText: "base 4294967295 plus size 2 exceeds the uint32 address space",
		},
	}

	database, cleanup := NewTestCluster(t)
	defer cleanup()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := database.Transaction(context.Background(), func(ctx context.Context, tx *ClusterTx) error {
				_, err := tx.GetIDMapUsage(ctx, owner, tt.base, tt.size)
				return err
			})
			require.ErrorContains(t, err, tt.errorText)
		})
	}
}

func TestGetIDMapUsageRejectsMalformedAndOverflowingConfig(t *testing.T) {
	const owner = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	tests := []struct {
		name      string
		profile   bool
		config    map[string]string
		errorText string
	}{
		{
			name: "invalid volatile base outside query window",
			config: map[string]string{
				"volatile.idmap.base": "not-a-number",
			},
			errorText: "Invalid instance ID map config",
		},
		{
			name: "negative local base",
			config: map[string]string{
				"security.idmap.base": "-1",
			},
			errorText: "Invalid instance ID map config",
		},
		{
			name: "zero local size",
			config: map[string]string{
				"security.idmap.size": "0",
			},
			errorText: "Invalid instance ID map config",
		},
		{
			name: "non-runtime auto spelling",
			config: map[string]string{
				"security.idmap.size": "AUTO",
			},
			errorText: "Invalid instance ID map config",
		},
		{
			name:    "profile base exceeds uint32",
			profile: true,
			config: map[string]string{
				"security.idmap.base": "4294967296",
			},
			errorText: "Invalid profile ID map config",
		},
		{
			name:    "profile size exceeds uint32",
			profile: true,
			config: map[string]string{
				"security.idmap.size": "4294967296",
			},
			errorText: "Invalid profile ID map config",
		},
		{
			name: "default range overflows uint32",
			config: map[string]string{
				"security.idmap.base": "4294901761",
			},
			errorText: "Invalid effective ID map range",
		},
		{
			name:    "custom profile range overflows uint32",
			profile: true,
			config: map[string]string{
				"security.idmap.base": "4294967295",
				"security.idmap.size": "2",
			},
			errorText: "Invalid effective ID map range",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			database, cleanup := NewTestCluster(t)
			defer cleanup()

			err := database.Transaction(context.Background(), func(ctx context.Context, tx *ClusterTx) error {
				if tt.profile {
					insertIDMapUsageProfile(t, tx, "malformed", tt.config)
				} else {
					insertIDMapUsageInstance(t, tx, "malformed", tt.config)
				}

				return nil
			})
			require.NoError(t, err)

			err = database.Transaction(context.Background(), func(ctx context.Context, tx *ClusterTx) error {
				_, err := tx.GetIDMapUsage(ctx, owner, 1000000, 65536)
				return err
			})
			require.ErrorContains(t, err, tt.errorText)
		})
	}
}

func insertIDMapUsageProfile(t *testing.T, tx *ClusterTx, name string, config map[string]string) int64 {
	t.Helper()
	result, err := tx.Tx().Exec(
		"INSERT INTO profiles (name, description, project_id) VALUES (?, '', 1)", name)
	require.NoError(t, err)
	id, err := result.LastInsertId()
	require.NoError(t, err)
	for key, value := range config {
		_, err = tx.Tx().Exec(
			"INSERT INTO profiles_config (profile_id, key, value) VALUES (?, ?, ?)", id, key, value)
		require.NoError(t, err)
	}

	return id
}

func insertIDMapUsageInstance(t *testing.T, tx *ClusterTx, name string, config map[string]string) int64 {
	t.Helper()
	result, err := tx.Tx().Exec(
		"INSERT INTO instances (node_id, name, architecture, type, project_id, description) VALUES (1, ?, 1, 0, 1, '')",
		name)
	require.NoError(t, err)
	id, err := result.LastInsertId()
	require.NoError(t, err)
	for key, value := range config {
		_, err = tx.Tx().Exec(
			"INSERT INTO instances_config (instance_id, key, value) VALUES (?, ?, ?)", id, key, value)
		require.NoError(t, err)
	}

	return id
}

func insertIDMapUsageInstanceProfile(t *testing.T, tx *ClusterTx, instanceID int64, profileID int64, order int) {
	t.Helper()
	_, err := tx.Tx().Exec(
		"INSERT INTO instances_profiles (instance_id, profile_id, apply_order) VALUES (?, ?, ?)",
		instanceID, profileID, order)
	require.NoError(t, err)
}

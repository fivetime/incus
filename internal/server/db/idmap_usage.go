//go:build linux && cgo && !agent

package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// IDMapUsageResource identifies an instance or profile matching an ID map owner or range.
type IDMapUsageResource struct {
	Type    string `json:"type"`
	Project string `json:"project"`
	Name    string `json:"name"`
}

func (c *ClusterTx) validateIDMapUsageConfig(ctx context.Context) error {
	const instanceStmt = `
SELECT projects.name, instances.name, instances_config.key, instances_config.value
  FROM instances_config INDEXED BY instances_config_idmap_malformed_usage_idx
  JOIN instances ON instances.id = instances_config.instance_id
  JOIN projects ON projects.id = instances.project_id
 WHERE (
       (instances_config.key IN ('security.idmap.base', 'volatile.idmap.base')
        AND instances_config.value != ''
        AND ((CASE WHEN substr(instances_config.value, 1, 1) = '+' THEN substr(instances_config.value, 2) ELSE instances_config.value END) = ''
             OR (CASE WHEN substr(instances_config.value, 1, 1) = '+' THEN substr(instances_config.value, 2) ELSE instances_config.value END) GLOB '*[^0-9]*'
             OR CAST(instances_config.value AS INTEGER) < 0
             OR CAST(instances_config.value AS INTEGER) > 4294967295))
       OR
       (instances_config.key = 'security.idmap.size'
        AND instances_config.value NOT IN ('', 'auto')
        AND ((CASE WHEN substr(instances_config.value, 1, 1) = '+' THEN substr(instances_config.value, 2) ELSE instances_config.value END) = ''
             OR (CASE WHEN substr(instances_config.value, 1, 1) = '+' THEN substr(instances_config.value, 2) ELSE instances_config.value END) GLOB '*[^0-9]*'
             OR CAST(instances_config.value AS INTEGER) <= 0
             OR CAST(instances_config.value AS INTEGER) > 4294967295))
       )
 LIMIT 1
`
	const profileStmt = `
SELECT projects.name, profiles.name, profiles_config.key, profiles_config.value
  FROM profiles_config INDEXED BY profiles_config_idmap_malformed_usage_idx
  JOIN profiles ON profiles.id = profiles_config.profile_id
  JOIN projects ON projects.id = profiles.project_id
 WHERE (
       (profiles_config.key = 'security.idmap.base'
        AND profiles_config.value != ''
        AND ((CASE WHEN substr(profiles_config.value, 1, 1) = '+' THEN substr(profiles_config.value, 2) ELSE profiles_config.value END) = ''
             OR (CASE WHEN substr(profiles_config.value, 1, 1) = '+' THEN substr(profiles_config.value, 2) ELSE profiles_config.value END) GLOB '*[^0-9]*'
             OR CAST(profiles_config.value AS INTEGER) < 0
             OR CAST(profiles_config.value AS INTEGER) > 4294967295))
       OR
       (profiles_config.key = 'security.idmap.size'
        AND profiles_config.value NOT IN ('', 'auto')
        AND ((CASE WHEN substr(profiles_config.value, 1, 1) = '+' THEN substr(profiles_config.value, 2) ELSE profiles_config.value END) = ''
             OR (CASE WHEN substr(profiles_config.value, 1, 1) = '+' THEN substr(profiles_config.value, 2) ELSE profiles_config.value END) GLOB '*[^0-9]*'
             OR CAST(profiles_config.value AS INTEGER) <= 0
             OR CAST(profiles_config.value AS INTEGER) > 4294967295))
       )
 LIMIT 1
`

	check := func(stmt string, resourceType string) error {
		var projectName string
		var resourceName string
		var key string
		var value string
		err := c.tx.QueryRowContext(ctx, stmt).Scan(&projectName, &resourceName, &key, &value)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}

		if err != nil {
			return fmt.Errorf("Query malformed ID map config: %w", err)
		}

		return fmt.Errorf("Invalid %s ID map config %q=%q on project %q resource %q", resourceType, key, value, projectName, resourceName)
	}

	err := check(instanceStmt, "instance")
	if err != nil {
		return err
	}

	return check(profileStmt, "profile")
}

// GetIDMapUsage returns resources whose effective ID map owner or range matches.
func (c *ClusterTx) GetIDMapUsage(ctx context.Context, owner string, base uint64, size uint64) ([]IDMapUsageResource, error) {
	parsedOwner, err := uuid.Parse(owner)
	if err != nil {
		return nil, fmt.Errorf("Parse ID map owner: %w", err)
	}

	const maxIDMapID = uint64(1<<32 - 1)
	const idMapLimit = maxIDMapID + 1
	if size == 0 {
		return nil, errors.New("Invalid ID map query range: size must be greater than zero")
	}

	if base > maxIDMapID {
		return nil, fmt.Errorf("Invalid ID map query range: base %d exceeds the uint32 maximum", base)
	}

	if size > maxIDMapID {
		return nil, fmt.Errorf("Invalid ID map query range: size %d exceeds the uint32 maximum", size)
	}

	if base > idMapLimit-size {
		return nil, fmt.Errorf("Invalid ID map query range: base %d plus size %d exceeds the uint32 address space", base, size)
	}

	ownerKey := strings.ReplaceAll(parsedOwner.String(), "-", "")
	err = c.validateIDMapUsageConfig(ctx)
	if err != nil {
		return nil, err
	}

	const stmt = `
WITH candidate_instances(id) AS (
    SELECT instance_id
      FROM instances_config
     WHERE key = 'user.openstack.uuid'
       AND replace(replace(replace(replace(lower(value), '-', ''), '{', ''), '}', ''), 'urn:uuid:', '') = ?1
    UNION
    SELECT instance_id
      FROM instances_config
     WHERE key = 'security.idmap.base'
       AND CAST(value AS INTEGER) >= ?4
       AND CAST(value AS INTEGER) < ?3
    UNION
    SELECT instance_id
      FROM instances_config
     WHERE key = 'security.idmap.base'
       AND CAST(value AS INTEGER) > ?5
    UNION
    SELECT instance_id
      FROM instances_config
     WHERE key = 'volatile.idmap.base'
       AND CAST(value AS INTEGER) >= ?4
       AND CAST(value AS INTEGER) < ?3
    UNION
    SELECT instance_id
      FROM instances_config
     WHERE key = 'volatile.idmap.base'
       AND CAST(value AS INTEGER) > ?5
    UNION
    SELECT instance_id
      FROM instances_config
     WHERE key = 'security.idmap.size'
       AND value NOT IN ('', 'auto', '65536')
    UNION
    SELECT ip.instance_id
      FROM profiles_config AS pc
      JOIN instances_profiles AS ip ON ip.profile_id = pc.profile_id
     WHERE pc.key = 'user.openstack.uuid'
       AND replace(replace(replace(replace(lower(pc.value), '-', ''), '{', ''), '}', ''), 'urn:uuid:', '') = ?1
    UNION
    SELECT ip.instance_id
      FROM profiles_config AS pc
      JOIN instances_profiles AS ip ON ip.profile_id = pc.profile_id
     WHERE pc.key = 'security.idmap.base'
       AND CAST(pc.value AS INTEGER) >= ?4
       AND CAST(pc.value AS INTEGER) < ?3
    UNION
    SELECT ip.instance_id
      FROM profiles_config AS pc
      JOIN instances_profiles AS ip ON ip.profile_id = pc.profile_id
     WHERE pc.key = 'security.idmap.base'
       AND CAST(pc.value AS INTEGER) > ?5
    UNION
    SELECT ip.instance_id
      FROM profiles_config AS pc
      JOIN instances_profiles AS ip ON ip.profile_id = pc.profile_id
     WHERE pc.key = 'security.idmap.size'
       AND pc.value NOT IN ('', 'auto', '65536')
),
effective_instances(id, project_id, name, owner, base, size) AS (
    SELECT i.id, i.project_id, i.name,
           COALESCE(
               (SELECT ic.value
                  FROM instances_config AS ic
                 WHERE ic.instance_id = i.id
                   AND ic.key = 'user.openstack.uuid'),
               (SELECT pc.value
                  FROM instances_profiles AS ip
                  JOIN profiles_config AS pc ON pc.profile_id = ip.profile_id
                 WHERE ip.instance_id = i.id
                   AND pc.key = 'user.openstack.uuid'
                 ORDER BY ip.apply_order DESC
                 LIMIT 1)),
           COALESCE(
                (SELECT NULLIF(ic.value, '')
                  FROM instances_config AS ic
                 WHERE ic.instance_id = i.id
                   AND ic.key = 'volatile.idmap.base'),
               (SELECT ic.value
                  FROM instances_config AS ic
                 WHERE ic.instance_id = i.id
                   AND ic.key = 'security.idmap.base'),
               (SELECT pc.value
                  FROM instances_profiles AS ip
                  JOIN profiles_config AS pc ON pc.profile_id = ip.profile_id
                 WHERE ip.instance_id = i.id
                   AND pc.key = 'security.idmap.base'
                 ORDER BY ip.apply_order DESC
                 LIMIT 1)),
           COALESCE(
               (SELECT ic.value
                  FROM instances_config AS ic
                 WHERE ic.instance_id = i.id
                   AND ic.key = 'security.idmap.size'),
               (SELECT pc.value
                  FROM instances_profiles AS ip
                  JOIN profiles_config AS pc ON pc.profile_id = ip.profile_id
                 WHERE ip.instance_id = i.id
                   AND pc.key = 'security.idmap.size'
                 ORDER BY ip.apply_order DESC
                 LIMIT 1))
      FROM candidate_instances AS candidates
      JOIN instances AS i ON i.id = candidates.id
),
normalized_instances(id, project_id, name, owner, base, size) AS (
    SELECT id, project_id, name, owner,
           CAST(NULLIF(base, '') AS INTEGER),
           CASE
               WHEN size IS NULL OR size IN ('', 'auto') THEN 65536
               ELSE CAST(size AS INTEGER)
           END
      FROM effective_instances
),
invalid_instances(id, project_id, name, base, size) AS (
    SELECT id, project_id, name, base, size
      FROM normalized_instances
     WHERE base IS NOT NULL
       AND (base < 0 OR base > 4294967295
            OR size IS NULL OR size <= 0 OR size > 4294967295
            OR base + size > 4294967296)
),
matching_instances(id, project_id, name) AS (
    SELECT id, project_id, name
      FROM normalized_instances
     WHERE replace(replace(replace(replace(lower(owner), '-', ''), '{', ''), '}', ''), 'urn:uuid:', '') = ?1
        OR (base >= 0 AND base < ?3
            AND size > 0 AND size <= 4294967296
            AND base + size > ?2
            AND base + size <= 4294967296)
),
candidate_profiles(id) AS (
    SELECT profile_id
      FROM profiles_config
     WHERE key = 'user.openstack.uuid'
       AND replace(replace(replace(replace(lower(value), '-', ''), '{', ''), '}', ''), 'urn:uuid:', '') = ?1
    UNION
    SELECT profile_id
      FROM profiles_config
     WHERE key = 'security.idmap.base'
       AND CAST(value AS INTEGER) >= ?4
       AND CAST(value AS INTEGER) < ?3
    UNION
    SELECT profile_id
      FROM profiles_config
     WHERE key = 'security.idmap.base'
       AND CAST(value AS INTEGER) > ?5
    UNION
    SELECT profile_id
      FROM profiles_config
     WHERE key = 'security.idmap.size'
       AND value NOT IN ('', 'auto', '65536')
),
effective_profiles(id, project_id, name, owner, base, size) AS (
    SELECT p.id, p.project_id, p.name,
           (SELECT pc.value
              FROM profiles_config AS pc
             WHERE pc.profile_id = p.id
               AND pc.key = 'user.openstack.uuid'),
           (SELECT pc.value
              FROM profiles_config AS pc
             WHERE pc.profile_id = p.id
               AND pc.key = 'security.idmap.base'),
           (SELECT pc.value
              FROM profiles_config AS pc
             WHERE pc.profile_id = p.id
               AND pc.key = 'security.idmap.size')
      FROM candidate_profiles AS candidates
      JOIN profiles AS p ON p.id = candidates.id
),
normalized_profiles(id, project_id, name, owner, base, size) AS (
    SELECT id, project_id, name, owner,
           CAST(NULLIF(base, '') AS INTEGER),
           CASE
               WHEN size IS NULL OR size IN ('', 'auto') THEN 65536
               ELSE CAST(size AS INTEGER)
           END
      FROM effective_profiles
),
invalid_profiles(id, project_id, name, base, size) AS (
    SELECT id, project_id, name, base, size
      FROM normalized_profiles
     WHERE base IS NOT NULL
       AND (base < 0 OR base > 4294967295
            OR size IS NULL OR size <= 0 OR size > 4294967295
            OR base + size > 4294967296)
),
matching_profiles(id, project_id, name) AS (
    SELECT id, project_id, name
      FROM normalized_profiles
     WHERE replace(replace(replace(replace(lower(owner), '-', ''), '{', ''), '}', ''), 'urn:uuid:', '') = ?1
        OR (base >= 0 AND base < ?3
            AND size > 0 AND size <= 4294967296
            AND base + size > ?2
            AND base + size <= 4294967296)
)
SELECT 0, 'instance', projects.name, invalid_instances.name,
       invalid_instances.base, invalid_instances.size
  FROM invalid_instances
  JOIN projects ON projects.id = invalid_instances.project_id
UNION ALL
SELECT 0, 'profile', projects.name, invalid_profiles.name,
       invalid_profiles.base, invalid_profiles.size
  FROM invalid_profiles
  JOIN projects ON projects.id = invalid_profiles.project_id
UNION ALL
SELECT 1, 'instance', projects.name, matching_instances.name, NULL, NULL
  FROM matching_instances
  JOIN projects ON projects.id = matching_instances.project_id
UNION ALL
SELECT 1, 'profile', projects.name, matching_profiles.name, NULL, NULL
  FROM matching_profiles
  JOIN projects ON projects.id = matching_profiles.project_id
ORDER BY 1, 2, 3, 4
`

	// The allocator's normal 65536-wide geometry is selected through a
	// bounded start-range index. Resources that explicitly configure another
	// size are kept in covering partial indexes and conservatively rechecked.
	// Their path is proportional to the custom-size set and profile fan-out.
	const defaultSize = uint64(65536)
	const rangeLimit = uint64(1) << 32
	lowerBound := uint64(0)
	if base >= defaultSize {
		lowerBound = base - defaultSize + 1
	}

	rows, err := c.tx.QueryContext(
		ctx, stmt, ownerKey, int64(base), int64(base+size), int64(lowerBound), int64(rangeLimit-defaultSize))
	if err != nil {
		return nil, fmt.Errorf("Query ID map usage: %w", err)
	}

	defer func() { _ = rows.Close() }()

	resources := make([]IDMapUsageResource, 0)
	for rows.Next() {
		var resultKind int
		resource := IDMapUsageResource{}
		var invalidBase sql.NullInt64
		var invalidSize sql.NullInt64
		err = rows.Scan(&resultKind, &resource.Type, &resource.Project, &resource.Name, &invalidBase, &invalidSize)
		if err != nil {
			return nil, fmt.Errorf("Scan ID map usage: %w", err)
		}

		if resultKind == 0 {
			return nil, fmt.Errorf("Invalid effective ID map range on %s %q in project %q: base=%d size=%d", resource.Type, resource.Name, resource.Project, invalidBase.Int64, invalidSize.Int64)
		}

		if resultKind != 1 {
			return nil, fmt.Errorf("Invalid ID map usage result kind %d", resultKind)
		}

		resources = append(resources, resource)
	}

	err = rows.Err()
	if err != nil {
		return nil, fmt.Errorf("Read ID map usage: %w", err)
	}

	return resources, nil
}

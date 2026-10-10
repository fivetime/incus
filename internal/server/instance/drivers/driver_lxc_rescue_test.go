//go:build linux && cgo && !agent

package drivers

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/lxc/incus/v7/internal/server/db"
	"github.com/lxc/incus/v7/internal/server/state"
	"github.com/lxc/incus/v7/internal/server/storage/rescue"
	"github.com/lxc/incus/v7/shared/api"
	"github.com/lxc/incus/v7/shared/osarch"
)

func rescueTemplateDriver(t *testing.T) *lxc {
	t.Helper()
	t.Setenv("INCUS_DIR", t.TempDir())
	database, cleanup := db.NewTestCluster(t)
	t.Cleanup(cleanup)

	config := map[string]string{
		rescueTokenKey:              "a41feaa1-a4c0-41a5-aa9d-f9aa54c25ff9",
		rescuePhaseKey:              "preparing",
		"volatile.apply_template":   "copy",
		"volatile.last_state.idmap": fmt.Sprintf(`[{"Isuid":true,"Hostid":%d,"Nsid":0,"Maprange":1},{"Isgid":true,"Hostid":%d,"Nsid":0,"Maprange":1}]`, os.Geteuid(), os.Getegid()),
		"user.user-data":            "#cloud-config\nrescue: true",
		"user.network-config":       "version: 2\nethernets: {eth0: {dhcp4: true}}",
	}

	d := &lxc{common: common{
		state: &state.State{DB: &db.DB{Cluster: database}},
		name:  "rescue-template", architecture: osarch.ARCH_64BIT_INTEL_X86,
		project:     api.Project{Name: "default"},
		localConfig: config, expandedConfig: maps.Clone(config),
	}}
	err := database.Transaction(context.Background(), func(ctx context.Context, tx *db.ClusterTx) error {
		result, err := tx.Tx().Exec(`INSERT INTO instances(node_id, name, architecture, type, project_id, description) VALUES (1, ?, ?, 0, 1, '')`, d.name, d.architecture)
		if err != nil {
			return err
		}

		id, err := result.LastInsertId()
		if err != nil {
			return err
		}

		d.id = int(id)
		return tx.UpdateInstanceConfig(d.id, config)
	})
	require.NoError(t, err)

	original := d.common.RootfsPath()
	require.NoError(t, os.MkdirAll(original, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(original, "original"), []byte("original root"), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(d.Path(), "templates"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(d.Path(), "metadata.yaml"), []byte("templates:\n  /original:\n    when: [create, copy, start]\n    template: original.tpl\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(d.Path(), "templates", "original.tpl"), []byte("wrong original template"), 0o600))

	err = rescue.Prepare(d.Path(), config[rescueTokenKey], strings.Repeat("a", 64), func(path string) error {
		err := os.MkdirAll(filepath.Join(path, "rootfs"), 0o755)
		if err != nil {
			return err
		}

		err = os.Mkdir(filepath.Join(path, "templates"), 0o755)
		if err != nil {
			return err
		}

		metadata := "templates:\n"
		for name, content := range map[string]string{
			"meta-data":      "instance-id: rescue-{{ instance.name }}\n",
			"user-data":      `{{ config_get("user.user-data", "") }}`,
			"network-config": `{{ config_get("user.network-config", "") }}`,
		} {
			metadata += fmt.Sprintf("  /var/lib/cloud/seed/nocloud-net/%s:\n    when: [create, copy]\n    template: %s.tpl\n", name, name)
			err = os.WriteFile(filepath.Join(path, "templates", name+".tpl"), []byte(content), 0o600)
			if err != nil {
				return err
			}
		}

		return os.WriteFile(filepath.Join(path, "metadata.yaml"), []byte(metadata), 0o600)
	})
	require.NoError(t, err)
	return d
}

func rescueTemplateConfig(t *testing.T, d *lxc) map[string]string {
	t.Helper()
	config := map[string]string{}
	err := d.state.DB.Cluster.Transaction(context.Background(), func(ctx context.Context, tx *db.ClusterTx) error {
		rows, err := tx.Tx().Query("SELECT key, value FROM instances_config WHERE instance_id=?", d.id)
		if err != nil {
			return err
		}

		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var key, value string
			err = rows.Scan(&key, &value)
			if err != nil {
				return err
			}

			config[key] = value
		}

		return rows.Err()
	})
	require.NoError(t, err)
	return config
}

func TestRescueStartTemplatesInitializeOnlyTemporaryRootOnce(t *testing.T) {
	d := rescueTemplateDriver(t)
	require.NoError(t, d.activateRescue())
	config := rescueTemplateConfig(t, d)
	require.Equal(t, "active", config[rescuePhaseKey])
	require.Equal(t, "create", config[rescueTemplateKey])
	require.Equal(t, "copy", config["volatile.apply_template"])

	d.localConfig, d.expandedConfig = config, maps.Clone(config)
	require.NoError(t, d.applyStartTemplates())
	seed := filepath.Join(d.RootfsPath(), "var/lib/cloud/seed/nocloud-net")
	for name, expected := range map[string]string{
		"meta-data":      "instance-id: rescue-rescue-template\n",
		"user-data":      config["user.user-data"],
		"network-config": config["user.network-config"],
	} {
		actual, err := os.ReadFile(filepath.Join(seed, name))
		require.NoError(t, err)
		require.Equal(t, expected, string(actual))
	}

	config = rescueTemplateConfig(t, d)
	require.Empty(t, config[rescueTemplateKey])
	require.Equal(t, "copy", config["volatile.apply_template"])
	d.localConfig, d.expandedConfig = config, maps.Clone(config)
	d.expandedConfig["user.user-data"] = "must not rerender on retry"
	require.NoError(t, d.activateRescue())
	require.NoError(t, d.applyStartTemplates())
	actual, err := os.ReadFile(filepath.Join(seed, "user-data"))
	require.NoError(t, err)
	require.Equal(t, config["user.user-data"], string(actual))
	actual, err = os.ReadFile(filepath.Join(d.common.RootfsPath(), "original"))
	require.NoError(t, err)
	require.Equal(t, "original root", string(actual))
	_, err = os.Stat(filepath.Join(d.common.RootfsPath(), "var/lib/cloud/seed/nocloud-net"))
	require.True(t, os.IsNotExist(err))
}

func TestRescueStartTemplatesRetainPendingTriggerOnFailure(t *testing.T) {
	for _, failure := range []string{"render", "persist"} {
		t.Run(failure, func(t *testing.T) {
			d := rescueTemplateDriver(t)
			require.NoError(t, d.activateRescue())
			template := filepath.Join(d.TemplatesPath(), "user-data.tpl")
			if failure == "render" {
				require.NoError(t, os.WriteFile(template, []byte("{% invalid_template_tag %}"), 0o600))
			} else {
				_, err := d.state.DB.Cluster.DB().Exec(`CREATE TRIGGER fail_rescue_template_delete BEFORE DELETE ON instances_config WHEN OLD.key='volatile.rescue.apply_template' BEGIN SELECT RAISE(ABORT, 'forced template persistence failure'); END`)
				require.NoError(t, err)
			}

			require.Error(t, d.applyStartTemplates())
			config := rescueTemplateConfig(t, d)
			require.Equal(t, "create", config[rescueTemplateKey])
			require.Equal(t, "copy", config["volatile.apply_template"])
			if failure == "render" {
				require.NoError(t, os.WriteFile(template, []byte(`{{ config_get("user.user-data", "") }}`), 0o600))
			} else {
				_, err := d.state.DB.Cluster.DB().Exec("DROP TRIGGER fail_rescue_template_delete")
				require.NoError(t, err)
			}

			d.localConfig, d.expandedConfig = config, maps.Clone(config)
			require.NoError(t, d.applyStartTemplates())
			actual, err := os.ReadFile(filepath.Join(d.RootfsPath(), "var/lib/cloud/seed/nocloud-net/user-data"))
			require.NoError(t, err)
			require.Equal(t, config["user.user-data"], string(actual))
			actual, err = os.ReadFile(filepath.Join(d.common.RootfsPath(), "original"))
			require.NoError(t, err)
			require.Equal(t, "original root", string(actual))
		})
	}
}

func TestRescueActivationCommitsPhaseAndTemplateTogether(t *testing.T) {
	d := rescueTemplateDriver(t)
	_, err := d.state.DB.Cluster.DB().Exec(`CREATE TRIGGER fail_rescue_template_insert BEFORE INSERT ON instances_config WHEN NEW.key='volatile.rescue.apply_template' BEGIN SELECT RAISE(ABORT, 'forced activation failure'); END`)
	require.NoError(t, err)
	require.Error(t, d.activateRescue())
	config := rescueTemplateConfig(t, d)
	require.Equal(t, "preparing", config[rescuePhaseKey])
	require.Empty(t, config[rescueTemplateKey])
	require.Equal(t, "preparing", d.localConfig[rescuePhaseKey])
	require.Error(t, d.validateRescueRoot())

	_, err = d.state.DB.Cluster.DB().Exec("DROP TRIGGER fail_rescue_template_insert")
	require.NoError(t, err)
	d.localConfig, d.expandedConfig = config, maps.Clone(config)
	require.NoError(t, d.activateRescue())
	require.NoError(t, d.applyStartTemplates())
	config = rescueTemplateConfig(t, d)
	require.Equal(t, "active", config[rescuePhaseKey])
	require.Empty(t, config[rescueTemplateKey])
	require.Equal(t, "copy", config["volatile.apply_template"])
}

func TestStartTemplatesConsumeOrdinaryPendingTrigger(t *testing.T) {
	for _, trigger := range []string{"create", "copy"} {
		t.Run(trigger, func(t *testing.T) {
			d := rescueTemplateDriver(t)
			require.NoError(t, d.VolatileSet(map[string]string{
				rescueTokenKey: "", rescuePhaseKey: "", "volatile.apply_template": trigger,
			}))
			metadata := "templates:\n  /pending:\n    when: [create, copy]\n    template: pending.tpl\n  /started:\n    when: [start]\n    template: started.tpl\n"
			require.NoError(t, os.WriteFile(filepath.Join(d.Path(), "metadata.yaml"), []byte(metadata), 0o600))
			require.NoError(t, os.WriteFile(filepath.Join(d.TemplatesPath(), "pending.tpl"), []byte("{{ trigger }}"), 0o600))
			require.NoError(t, os.WriteFile(filepath.Join(d.TemplatesPath(), "started.tpl"), []byte("{{ trigger }}"), 0o600))

			require.NoError(t, d.applyStartTemplates())
			for path, expected := range map[string]string{"pending": trigger, "started": "start"} {
				actual, err := os.ReadFile(filepath.Join(d.RootfsPath(), path))
				require.NoError(t, err)
				require.Equal(t, expected, string(actual))
			}

			config := rescueTemplateConfig(t, d)
			require.Empty(t, config["volatile.apply_template"])
			d.localConfig, d.expandedConfig = config, maps.Clone(config)
			require.NoError(t, os.WriteFile(filepath.Join(d.RootfsPath(), "pending"), []byte("retained"), 0o600))
			require.NoError(t, d.applyStartTemplates())
			actual, err := os.ReadFile(filepath.Join(d.RootfsPath(), "pending"))
			require.NoError(t, err)
			require.Equal(t, "retained", string(actual))
			_, err = os.Stat(filepath.Join(rescue.ImagePath(d.Path()), "rootfs/var/lib/cloud/seed"))
			require.True(t, os.IsNotExist(err))
		})
	}
}

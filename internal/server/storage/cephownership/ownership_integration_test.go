//go:build linux && cgo

package cephownership

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
)

// TestNativeOwnershipCAS uses only uniquely named disposable images in an explicitly supplied test pool.
func TestNativeOwnershipCAS(t *testing.T) {
	pool := os.Getenv("INCUS_CEPH_OWNERSHIP_TEST_POOL")
	if pool == "" {
		t.Skip("No dedicated Ceph ownership test pool configured")
	}

	cluster := os.Getenv("INCUS_CEPH_OWNERSHIP_TEST_CLUSTER")
	user := os.Getenv("INCUS_CEPH_OWNERSHIP_TEST_USER")
	if cluster == "" || user == "" {
		t.Fatal("Test cluster and user must be explicit")
	}

	command := func(program string, args ...string) string {
		t.Helper()
		out, err := exec.Command(program, args...).CombinedOutput()
		if err != nil {
			t.Fatalf("%s failed: %v: %s", program, err, out)
		}

		return strings.TrimSpace(string(out))
	}

	rbd := func(args ...string) string {
		t.Helper()
		return command("rbd", append([]string{"--cluster", cluster, "--id", user, "--pool", pool}, args...)...)
	}

	idBytes := make([]byte, 12)
	_, err := rand.Read(idBytes)
	if err != nil {
		t.Fatal(err)
	}

	name := "incus_takeover_protocol_test_" + hex.EncodeToString(idBytes)
	rbd("create", name, "--size", "8M")
	names := []string{name}
	t.Cleanup(func() {
		for _, image := range names {
			out, err := exec.Command("rbd", "--cluster", cluster, "--id", user, "--pool", pool, "rm", image).CombinedOutput()
			if err != nil {
				t.Errorf("Remove disposable image %s: %v: %s", image, err, out)
			}
		}
	})
	info := struct {
		ID string `json:"id"`
	}{}
	err = json.Unmarshal([]byte(rbd("info", name, "--format", "json")), &info)
	if err != nil {
		t.Fatal(err)
	}

	pools := []struct {
		PoolName string `json:"poolname"`
		PoolID   int64  `json:"poolnum"`
	}{}
	err = json.Unmarshal([]byte(command("ceph", "--cluster", cluster, "--id", user, "osd", "lspools", "--format", "json")), &pools)
	if err != nil {
		t.Fatal(err)
	}

	poolID := int64(-1)
	for _, item := range pools {
		if item.PoolName == pool {
			poolID = item.PoolID
		}
	}

	binding := Binding{Cluster: cluster, User: user, FSID: command("ceph", "--cluster", cluster, "--id", user, "fsid"), PoolID: poolID, ImageID: info.ID}
	old := "sha256:" + strings.Repeat("a", 64)
	next := []string{"sha256:" + strings.Repeat("b", 64), "sha256:" + strings.Repeat("c", 64)}
	rbd("image-meta", "set", name, "incus.openstack.materialization_ownership", old)

	var wait sync.WaitGroup
	results := make([]error, 2)
	for i := range next {
		wait.Add(1)
		go func(i int) {
			defer wait.Done()
			results[i] = Transfer(binding, old, next[i])
		}(i)
	}

	wait.Wait()
	winner := -1
	for i, result := range results {
		if result == nil {
			if winner >= 0 {
				t.Fatal("Two ownership transfers succeeded")
			}

			winner = i
		}
	}

	if winner < 0 {
		t.Fatalf("No ownership transfer succeeded: %v", results)
	}

	replayErr := Transfer(binding, old, next[winner])
	if replayErr != nil {
		t.Fatalf("Lost-response replay failed: %v", replayErr)
	}

	currentOwner := rbd("image-meta", "get", name, "incus.openstack.materialization_ownership")
	if currentOwner != next[winner] {
		t.Fatalf("Unexpected owner %q", currentOwner)
	}

	wrongCluster := binding
	wrongCluster.FSID = "00000000-0000-0000-0000-000000000000"
	if Transfer(wrongCluster, next[winner], old) == nil {
		t.Fatal("Wrong cluster accepted")
	}

	missingImage := binding
	missingImage.ImageID = "ffffffffffffffffffffffff"
	if Transfer(missingImage, old, next[winner]) == nil {
		t.Fatal("Missing image header was created")
	}

	// Replace only the name; an identity-bound transfer must never touch the replacement.
	retained := name + "_retained"
	rbd("rename", name, retained)
	names[0] = retained
	rbd("create", name, "--size", "8M")
	names = append(names, name)
	replacementMarker := "sha256:" + strings.Repeat("d", 64)
	rbd("image-meta", "set", name, "incus.openstack.materialization_ownership", replacementMarker)
	err = Transfer(binding, next[winner], old)
	if err != nil {
		t.Fatal(err)
	}

	replacementOwner := rbd("image-meta", "get", name, "incus.openstack.materialization_ownership")
	if replacementOwner != replacementMarker {
		t.Fatalf("Replacement marker changed: %q", replacementOwner)
	}

	retainedOwner := rbd("image-meta", "get", retained, "incus.openstack.materialization_ownership")
	if retainedOwner != old {
		t.Fatal("Original image was not transferred")
	}

	// Flatten changes the same RBD header while background storage work can
	// overlap a fenced takeover. The identity-bound metadata CAS must remain
	// valid and must not redirect to the parent or a same-named image.
	flatParent := name + "_flatten_parent"
	flatChild := name + "_flatten_child"
	rbd("create", flatParent, "--size", "8M")
	rbd("snap", "create", flatParent+"@base")
	rbd("snap", "protect", flatParent+"@base")
	rbd("clone", pool+"/"+flatParent+"@base", pool+"/"+flatChild)
	t.Cleanup(func() {
		for _, args := range [][]string{
			{"rm", flatChild},
			{"snap", "unprotect", flatParent + "@base"},
			{"snap", "rm", flatParent + "@base"},
			{"rm", flatParent},
		} {
			out, cleanupErr := exec.Command(
				"rbd", append([]string{
					"--cluster", cluster, "--id", user,
					"--pool", pool,
				}, args...)...).CombinedOutput()
			if cleanupErr != nil {
				t.Errorf("Flatten fixture cleanup %v: %v: %s", args, cleanupErr, out)
			}
		}
	})
	flatInfo := struct {
		ID string `json:"id"`
	}{}
	err = json.Unmarshal([]byte(rbd("info", flatChild, "--format", "json")), &flatInfo)
	if err != nil {
		t.Fatal(err)
	}

	flatBinding := binding
	flatBinding.ImageID = flatInfo.ID
	flatOld := "sha256:" + strings.Repeat("e", 64)
	flatNext := "sha256:" + strings.Repeat("f", 64)
	rbd("image-meta", "set", flatChild, "incus.openstack.materialization_ownership", flatOld)
	ready := make(chan struct{})
	flattenResults := make([]error, 2)
	wait.Add(2)
	go func() {
		defer wait.Done()
		<-ready
		_, flattenResults[0] = exec.Command(
			"rbd", "--cluster", cluster, "--id", user, "--pool", pool,
			"flatten", flatChild).CombinedOutput()
	}()
	go func() {
		defer wait.Done()
		<-ready
		flattenResults[1] = Transfer(flatBinding, flatOld, flatNext)
	}()
	close(ready)
	wait.Wait()
	if flattenResults[0] != nil || flattenResults[1] != nil {
		t.Fatalf("Concurrent flatten/transfer failed: %v", flattenResults)
	}

	flattenedOwner := rbd("image-meta", "get", flatChild, "incus.openstack.materialization_ownership")
	if flattenedOwner != flatNext {
		t.Fatalf("Flatten race changed owner: %q", flattenedOwner)
	}

	flatAfter := struct {
		ID string `json:"id"`
	}{}
	err = json.Unmarshal([]byte(rbd("info", flatChild, "--format", "json")), &flatAfter)
	if err != nil || flatAfter.ID != flatInfo.ID {
		t.Fatalf("Flatten changed immutable identity: %q, %v", flatAfter.ID, err)
	}

	t.Logf("Atomic competition, response replay, FSID rejection, name replacement and flatten race passed for image ID %s", info.ID)
}

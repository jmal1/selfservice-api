package database

import "testing"

import "github.com/jmal1/selfservice-api/internal/models"

func TestDestroySkipsVLANOwnershipOnlyForShared(t *testing.T) {
	if !destroySkipsVLANOwnership(models.NetworkModeShared) {
		t.Fatal("shared Single VM destroy must not require a vlan_pool row")
	}
	if destroySkipsVLANOwnership(models.NetworkModeIsolated) || destroySkipsVLANOwnership("") {
		t.Fatal("isolated destroy must keep the exact VLAN ownership check")
	}
}

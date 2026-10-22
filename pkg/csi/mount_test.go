package csi

import (
	"reflect"
	"strings"
	"testing"
)

func TestMountPoints(t *testing.T) {
	info := `22 1 8:1 / / rw,relatime shared:1 - ext4 /dev/sda1 rw
610 22 8:1 /var/lib/onggi/artifacts/ab /var/lib/kubelet/pods/u1/volumes/kubernetes.io~csi/model/mount ro,nosuid,nodev,relatime shared:1 - ext4 /dev/sda1 rw
611 22 0:52 / /mnt/with\040space rw - tmpfs tmpfs rw
`
	got, err := mountPoints(strings.NewReader(info))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/", "/var/lib/kubelet/pods/u1/volumes/kubernetes.io~csi/model/mount", "/mnt/with space"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

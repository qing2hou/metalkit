package images

import "testing"

func TestDetectFromFilename(t *testing.T) {
	cases := []struct {
		name        string
		wantFamily  string
		wantVersion string
	}{
		// Ubuntu cloud images — official upstream naming.
		{"jammy-server-cloudimg-amd64.img", "ubuntu", "22.04"},
		{"focal-server-cloudimg-amd64.qcow2", "ubuntu", "20.04"},
		{"noble-server-cloudimg-amd64.img", "ubuntu", "24.04"},
		{"ubuntu-22.04-server-cloudimg-amd64.qcow2", "ubuntu", "22.04"},
		{"ubuntu-20.04.6-server-cloudimg-amd64.img", "ubuntu", "20.04"},

		// Debian cloud images.
		{"debian-12-genericcloud-amd64.qcow2", "debian", "12"},
		{"debian-11-genericcloud-amd64.qcow2", "debian", "11"},
		{"debian-bookworm-genericcloud-amd64.qcow2", "debian", "12"},
		{"debian-bullseye-generic-amd64.qcow2", "debian", "11"},

		// CentOS 7 special-case (rhel7 family).
		{"CentOS-7-x86_64-GenericCloud-2009.qcow2", "rhel7", "7"},
		{"centos-7.9-azure.qcow2", "rhel7", "7.9"},

		// Rocky / Alma / RHEL / Oracle / Fedora → rhel.
		{"Rocky-9-GenericCloud-Base.latest.x86_64.qcow2", "rhel", "9"},
		{"Rocky-8.10-GenericCloud-Base.qcow2", "rhel", "8.10"},
		{"AlmaLinux-9-GenericCloud-latest.x86_64.qcow2", "rhel", "9"},
		{"rhel-9.4-x86_64-kvm.qcow2", "rhel", "9.4"},
		{"OL9U4_x86_64-kvm-b234.qcow2", "rhel", "9"},
		{"Fedora-Cloud-Base-40-1.14.x86_64.qcow2", "rhel", "40"},

		// Unknown → empty.
		{"my-golden-image.qcow2", "", ""},
		{"", "", ""},
		{"server.img", "", ""},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := DetectFromFilename(c.name)
			if got.Family != c.wantFamily {
				t.Errorf("family: got %q, want %q", got.Family, c.wantFamily)
			}
			if got.Version != c.wantVersion {
				t.Errorf("version: got %q, want %q", got.Version, c.wantVersion)
			}
		})
	}
}

func TestMergeDetected_OperatorWins(t *testing.T) {
	in := CreateUploadInput{Name: "x", Family: "ubuntu", Version: "22.04"}
	det := DetectionResult{Family: "debian", Version: "12"}
	out := MergeDetected(in, det)
	if out.Family != "ubuntu" || out.Version != "22.04" {
		t.Fatalf("operator-supplied values must win: got family=%q version=%q", out.Family, out.Version)
	}
}

func TestMergeDetected_DetectionFillsEmpty(t *testing.T) {
	in := CreateUploadInput{Name: "x"}
	det := DetectionResult{Family: "rhel", Version: "9"}
	out := MergeDetected(in, det)
	if out.Family != "rhel" || out.Version != "9" {
		t.Fatalf("detection must backfill empty: got family=%q version=%q", out.Family, out.Version)
	}
}

func TestMergeDetected_PartialBackfill(t *testing.T) {
	in := CreateUploadInput{Name: "x", Family: "ubuntu"}
	det := DetectionResult{Family: "debian", Version: "12"}
	out := MergeDetected(in, det)
	if out.Family != "ubuntu" || out.Version != "12" {
		t.Fatalf("partial backfill: family kept as ubuntu, version filled from det: got family=%q version=%q", out.Family, out.Version)
	}
}

func TestDetectArchFromFilename(t *testing.T) {
	cases := map[string]string{
		"jammy-server-cloudimg-amd64.img":        ArchAmd64,
		"Rocky-9-aarch64.qcow2":                  ArchArm64,
		"debian-12-genericcloud_AMD64.qcow2":     ArchAmd64,
		"ubuntu-22.04-server-cloudimg-arm64.img": ArchArm64,
		"centos-7-x86_64-GenericCloud.qcow2":     ArchAmd64,
		"mydisk.qcow2":                           "",     // unknown → empty
		"somex64thing.raw":                       "",     // x64 without separators → no match
		"kylin-v10-aarch64.img":                  ArchArm64,
	}
	for name, want := range cases {
		got := DetectFromFilename(name).Arch
		if got != want {
			t.Errorf("arch(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestMergeDetectedFillsArch(t *testing.T) {
	in := CreateUploadInput{Name: "x.qcow2"} // operator blank
	got := MergeDetected(in, DetectionResult{Family: "ubuntu", Arch: ArchArm64})
	if got.Arch != ArchArm64 {
		t.Errorf("backfill arch = %q, want arm64", got.Arch)
	}
	// Operator value wins over detection.
	in2 := CreateUploadInput{Name: "x-arm64.qcow2", Arch: ArchAmd64}
	got2 := MergeDetected(in2, DetectionResult{Arch: ArchArm64})
	if got2.Arch != ArchAmd64 {
		t.Errorf("operator arch overridden: %q", got2.Arch)
	}
}

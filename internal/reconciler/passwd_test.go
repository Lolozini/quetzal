package reconciler

import (
	"strconv"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// Egg images create their "container" user as 1000 and Quetzal runs eggs as
// 988, a uid nothing in the image knew. Valheim asks who it runs as, got no
// answer and segfaulted at every start. The pod now mounts a passwd and a
// group that name the uid it runs as, with the data directory as its home.
func TestEggPodKnowsItsUser(t *testing.T) {
	s, tmpl := testServerAndTemplate()
	tmpl.DataPath = "/home/container"
	pod := BuildDeployment(s, tmpl, "", nil).Spec.Template.Spec
	uid := strconv.FormatInt(*pod.SecurityContext.RunAsUser, 10)
	gid := strconv.FormatInt(*pod.SecurityContext.RunAsGroup, 10)

	cm := BuildPasswdConfigMap(s, tmpl)
	if cm == nil {
		t.Fatal("no passwd for a template that names no user")
	}
	if cm.Namespace != s.Namespace {
		t.Errorf("passwd in namespace %q, want %q", cm.Namespace, s.Namespace)
	}
	entry := func(file string, uidField int, id string) []string {
		var found []string
		for _, line := range strings.Split(strings.TrimSuffix(cm.Data[file], "\n"), "\n") {
			f := strings.Split(line, ":")
			if file == "passwd" && len(f) != 7 || file == "group" && len(f) != 4 {
				t.Fatalf("%s line %q has %d fields", file, line, len(f))
			}
			if f[uidField] == id {
				found = f
			}
		}
		return found
	}
	if u := entry("passwd", 2, uid); u == nil || u[0] != "container" || u[3] != gid || u[5] != "/home/container" {
		t.Errorf("passwd entry for uid %s = %q\n%s", uid, u, cm.Data["passwd"])
	}
	if g := entry("group", 2, gid); g == nil || g[0] != "container" {
		t.Errorf("group entry for gid %s = %q\n%s", gid, g, cm.Data["group"])
	}

	// Mounted over the image's own files, from that ConfigMap.
	mounts := map[string]corev1.VolumeMount{}
	for _, m := range pod.Containers[0].VolumeMounts {
		mounts[m.MountPath] = m
	}
	for path, key := range map[string]string{"/etc/passwd": "passwd", "/etc/group": "group"} {
		m, ok := mounts[path]
		if !ok || m.SubPath != key || !m.ReadOnly {
			t.Errorf("%s mount = %+v (present: %v)", path, m, ok)
			continue
		}
		var from string
		for _, v := range pod.Volumes {
			if v.Name == m.Name && v.ConfigMap != nil {
				from = v.ConfigMap.Name
			}
		}
		if from != cm.Name {
			t.Errorf("%s comes from ConfigMap %q, want %q", path, from, cm.Name)
		}
	}
}

// A template that names its user runs an image that knows it: its passwd is
// left alone.
func TestTemplateUserKeepsTheImagePasswd(t *testing.T) {
	s, tmpl := testServerAndTemplate()
	uid := int64(1000)
	tmpl.SecurityContext.RunAsUser = &uid
	if cm := BuildPasswdConfigMap(s, tmpl); cm != nil {
		t.Errorf("passwd built for a template that names its user: %+v", cm.Data)
	}
	pod := BuildDeployment(s, tmpl, "", nil).Spec.Template.Spec
	for _, m := range pod.Containers[0].VolumeMounts {
		if strings.HasPrefix(m.MountPath, "/etc/") {
			t.Errorf("mounted %s", m.MountPath)
		}
	}
	for _, v := range pod.Volumes {
		if v.Name == passwdVolume {
			t.Errorf("volume %s present", v.Name)
		}
	}
}

// The home comes from the template's data path, which whoever edits templates
// sets: it must not be able to add a field or an entry.
func TestPasswdHomeCannotAddEntries(t *testing.T) {
	s, tmpl := testServerAndTemplate()
	for _, p := range []string{"/srv\nroot2:x:0:0::/:/bin/sh", "/srv:/bin/evil", ""} {
		tmpl.DataPath = p
		got := BuildPasswdConfigMap(s, tmpl).Data["passwd"]
		if strings.Count(got, "\n") != 3 || !strings.Contains(got, "\ncontainer:x:988:988::/home/container:/bin/sh\n") {
			t.Errorf("data path %q gives\n%s", p, got)
		}
	}
}

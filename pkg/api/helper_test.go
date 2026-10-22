package api

import "testing"

func TestSamePod(t *testing.T) {
	ns := &Namespace{Name: "ns"}
	tests := []struct {
		name string
		a, b *PodInfo
		want bool
	}{
		{"same uid", &PodInfo{Name: "a", Namespace: ns, Uid: "1"}, &PodInfo{Name: "b", Namespace: ns, Uid: "1"}, true},
		{"different uid same name", &PodInfo{Name: "web-0", Namespace: ns, Uid: "1"}, &PodInfo{Name: "web-0", Namespace: ns, Uid: "2"}, false},
		{"no uid same name", &PodInfo{Name: "web-0", Namespace: ns}, &PodInfo{Name: "web-0", Namespace: ns, Uid: "2"}, true},
		{"no uid different namespace", &PodInfo{Name: "web-0", Namespace: &Namespace{Name: "other"}}, &PodInfo{Name: "web-0", Namespace: ns}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SamePod(tt.a, tt.b); got != tt.want {
				t.Errorf("SamePod() = %v, want %v", got, tt.want)
			}
		})
	}
}

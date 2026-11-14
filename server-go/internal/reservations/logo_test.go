package reservations

import "testing"

func TestLogoINIAndDirectory(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  int
	}{
		{"80000400=10", 10}, {" 80000400 = +1_0\n", 10}, {"80000400=１２", 12},
		{"80000400=1__0\n80000400=12", -1}, {"80000400=_10", -1}, {"80000400=10_", -1},
		{"other=12\u202880000400=10", 10}, {"80000400=invalid\n80000400=12", -1},
	} {
		if got := GetLogoID(tc.value, 32768, 1024); got != tc.want {
			t.Errorf("%q = %d, want %d", tc.value, got, tc.want)
		}
	}
	index := "a b c 8000_00a_abc_05.png\na b c 8000_00A_fff_05.png\na b c 8000_00A_fff_02.BMP"
	if got := GetLogoFileName(index, 32768, 10, 5); got != "8000_00a_abc_05.png" {
		t.Fatal(got)
	}
	if got := GetLogoFileName(index, 32768, 10, 2); got != "8000_00A_fff_02.BMP" {
		t.Fatal(got)
	}
	if got := GetLogoFileName(index, 32768, 10, 3); got != "" {
		t.Fatal(got)
	}
}

package tsinfo

import (
	"encoding/json"
	"os"
	"strconv"
	"testing"
)

// regionsTestData は Python 版 (TSInformation) から生成した期待値。
type regionsTestData struct {
	RegionIDToNames  map[string][]string `json:"region_id_to_names"`
	NetworkIDToNames map[string][]string `json:"network_id_to_names"`
}

// TestRegionsMatchPython は地域マッピングが Python 版と完全に一致することを検証する。
func TestRegionsMatchPython(t *testing.T) {
	raw, err := os.ReadFile("testdata/regions.json")
	if err != nil {
		t.Fatal(err)
	}
	var expected regionsTestData
	if err := json.Unmarshal(raw, &expected); err != nil {
		t.Fatal(err)
	}

	// ***** 逆引きマッピング (地域識別 → 地域名) が一致するか *****
	if len(regionIDToRegionNames) != len(expected.RegionIDToNames) {
		t.Fatalf("region count = %d, want %d", len(regionIDToRegionNames), len(expected.RegionIDToNames))
	}
	for regionIDText, expectedNames := range expected.RegionIDToNames {
		regionID, err := strconv.Atoi(regionIDText)
		if err != nil {
			t.Fatal(err)
		}
		actualNames := regionIDToRegionNames[regionID]
		if len(actualNames) != len(expectedNames) {
			t.Errorf("region %d: names = %v, want %v", regionID, actualNames, expectedNames)
			continue
		}
		for i := range expectedNames {
			if actualNames[i] != expectedNames[i] {
				t.Errorf("region %d: names = %v, want %v", regionID, actualNames, expectedNames)
				break
			}
		}
	}

	// ***** ネットワーク ID からの逆引きが一致するか (全ネットワーク ID を総当たり) *****
	checked := 0
	for networkID := 0x7000; networkID < 0x8000; networkID++ {
		expectedNames, shouldExist := expected.NetworkIDToNames[strconv.Itoa(networkID)]
		actualNames := GetRegionNamesFromNetworkID(networkID)
		if !shouldExist {
			if actualNames != nil {
				t.Errorf("network_id %d: names = %v, want nil", networkID, actualNames)
			}
			continue
		}
		checked++
		if len(actualNames) != len(expectedNames) {
			t.Errorf("network_id %d: names = %v, want %v", networkID, actualNames, expectedNames)
			continue
		}
		for i := range expectedNames {
			if actualNames[i] != expectedNames[i] {
				t.Errorf("network_id %d: names = %v, want %v", networkID, actualNames, expectedNames)
				break
			}
		}
	}
	if checked != len(expected.NetworkIDToNames) {
		t.Errorf("checked %d network ids, want %d", checked, len(expected.NetworkIDToNames))
	}

	// ***** 地デジ以外のネットワーク ID は nil になるか *****
	for _, networkID := range []int{0, 1, 4, 6, 8, 33000, 65535} {
		if names := GetRegionNamesFromNetworkID(networkID); names != nil {
			t.Errorf("network_id %d: names = %v, want nil", networkID, names)
		}
	}
}

// TestTerrestrialRegionToRegionIDs は地域名からの順引きを検証する。
func TestTerrestrialRegionToRegionIDs(t *testing.T) {
	if ids := TerrestrialRegionToRegionIDs("東京都"); len(ids) != 2 || ids[0] != 23 || ids[1] != 1 {
		t.Errorf("東京都 = %v, want [23 1]", ids)
	}
	if ids := TerrestrialRegionToRegionIDs("沖縄県"); len(ids) != 1 || ids[0] != 62 {
		t.Errorf("沖縄県 = %v, want [62]", ids)
	}
	if ids := TerrestrialRegionToRegionIDs("存在しない地域"); ids != nil {
		t.Errorf("unknown region = %v, want nil", ids)
	}
}

package twitter

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
)

//go:embed browser_assets/scripts.json
var browserScriptsJSON []byte

// browserScript は本家 Python の f-string を JSON 引数の位置を保って保存したもの。
type browserScript struct {
	Template    string   `json:"template"`
	Expressions []string `json:"expressions"`
}

var browserScripts = func() map[string]browserScript {
	var data struct {
		Scripts map[string]browserScript `json:"scripts"`
	}
	if err := json.Unmarshal(browserScriptsJSON, &data); err != nil {
		panic(err)
	}
	return data.Scripts
}()

func renderBrowserScript(name string, arguments ...any) (string, error) {
	script, ok := browserScripts[name]
	if !ok || len(arguments) != len(script.Expressions) {
		return "", fmt.Errorf("invalid browser script arguments: %s", name)
	}
	// 一度の置換で、本文に含まれるプレースホルダー風の文字列を再展開しない。
	replacements := make([]string, 0, 2*len(arguments))
	for index, value := range arguments {
		encoded, err := PythonJSON(value)
		if err != nil {
			return "", err
		}
		replacements = append(replacements, fmt.Sprintf("__KONOMITV_JSON_%d__", index), string(encoded))
	}
	return strings.NewReplacer(replacements...).Replace(script.Template), nil
}

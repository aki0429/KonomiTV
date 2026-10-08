package api

import (
	"fmt"
	"math/big"
)

// searchIntegerModulo は wire 符号化に必要な桁を Python の非負剰余で計算する。
func searchIntegerModulo(text string, modulus int64) int {
	value, _ := new(big.Int).SetString(text, 10)
	return int(value.Mod(value, big.NewInt(modulus)).Int64())
}

// edcbServiceID は Python の三つ組 bitwise 演算と signed64 wire 境界を扱う。
// schema では制限しないが、CtrlCmdUtil.to_bytes と同じく wire 範囲外は失敗させる。
func (service programSearchConditionServiceSchema) edcbServiceID() (int64, error) {
	values := []*big.Int{big.NewInt(int64(service.NetworkID)), big.NewInt(int64(service.TransportStreamID)), big.NewInt(int64(service.ServiceID))}
	for i, text := range []string{service.largeNetworkID, service.largeTransportStreamID, service.largeServiceID} {
		if text != "" {
			values[i], _ = new(big.Int).SetString(text, 10)
		}
	}
	values[0].Lsh(values[0], 32)
	values[1].Lsh(values[1], 16)
	result := new(big.Int).Or(values[0], values[1])
	result.Or(result, values[2])
	if !result.IsInt64() {
		return 0, fmt.Errorf("edcb: value is out of range for the field")
	}
	return result.Int64(), nil
}

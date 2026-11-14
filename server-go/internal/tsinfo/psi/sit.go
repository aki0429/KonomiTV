package psi

import "time"

// SITService はパーシャルTSのサービスと番組の記述子。
type SITService struct {
	ServiceID     uint16
	RunningStatus uint8
	Descriptors   []Descriptor
}

// SIT は選択情報セクション。SDT/EIT/TOTの代わりとなる番組情報と時計を保持する。
type SIT struct {
	Section
	Header                  TableHeader
	TransmissionDescriptors []Descriptor
	Services                []SITService
	Clock                   time.Time
	ClockValid              bool
}

// PartialTSTime はパーシャルトランスポートストリームタイム記述子の時刻。
type PartialTSTime struct {
	Start, Clock           time.Time
	StartValid, ClockValid bool
	Duration               time.Duration
	DurationValid          bool
}

// DecodePartialTSTime は0xC3記述子を読み、加減算のオフセットを適用する。
func DecodePartialTSTime(data []byte) (PartialTSTime, error) {
	var value PartialTSTime
	if len(data) < 13 {
		return value, ErrSection
	}
	value.Start, value.StartValid = DecodeJSTTime(data[1:6])
	value.Duration, value.DurationValid = DecodeDuration(data[6:9])
	offset, valid := DecodeDuration(data[9:12])
	if !valid {
		offset = 0
	}
	if data[12]&4 != 0 {
		offset = -offset
	}
	if value.StartValid {
		value.Start = value.Start.Add(offset)
	}
	if data[12]&1 != 0 {
		if len(data) < 18 {
			return value, ErrSection
		}
		value.Clock, value.ClockValid = DecodeJSTTime(data[13:18])
		if value.ClockValid {
			value.Clock = value.Clock.Add(offset)
		}
	}
	return value, nil
}

// DecodeSIT はCRCと各ループの境界を確認してSITを読み取る。
func DecodeSIT(section Section) (*SIT, error) {
	header, body, err := tableHeader(section, 0x7f)
	if err != nil {
		return nil, err
	}
	if len(body) < 2 {
		return nil, ErrSection
	}
	length := length12(body[:2])
	if length > len(body)-2 {
		return nil, ErrSection
	}
	descriptors, err := DecodeDescriptors(body[2 : 2+length])
	if err != nil {
		return nil, err
	}
	table := &SIT{Section: section, Header: header, TransmissionDescriptors: descriptors}
	body = body[2+length:]
	for len(body) > 0 {
		if len(body) < 4 {
			return nil, ErrSection
		}
		length := length12(body[2:4])
		if length > len(body)-4 {
			return nil, ErrSection
		}
		descriptors, err := DecodeDescriptors(body[4 : 4+length])
		if err != nil {
			return nil, err
		}
		table.Services = append(table.Services, SITService{ServiceID: u16(body[:2]), RunningStatus: body[2] >> 4 & 7, Descriptors: descriptors})
		body = body[4+length:]
	}
	all := append([]Descriptor(nil), table.TransmissionDescriptors...)
	for _, service := range table.Services {
		all = append(all, service.Descriptors...)
	}
	for _, descriptor := range all {
		if descriptor.Tag != 0xc3 {
			continue
		}
		value, err := DecodePartialTSTime(descriptor.Data)
		if err == nil && value.ClockValid {
			table.Clock, table.ClockValid = value.Clock, true
			break
		}
	}
	return table, nil
}

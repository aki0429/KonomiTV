package reservations

// このファイルは arib_genre_table.go に生成した ariblib.constants の
// CONTENT_TYPE / USER_TYPE を、ReservationConditionsRouter.py と同じ順序で参照するためのアクセサ。

// GenreMiddle はジャンルの中分類の 1 エントリ。
type GenreMiddle struct {
	Key  int
	Name string
}

// GenreMajor はジャンルの中分類の 1 エントリ。
type GenreMajor struct {
	Key    int
	Major  string
	Middle []GenreMiddle
}

// GenreMajors は ariblib.constants.CONTENT_TYPE 相当を挿入順で返す。
func GenreMajors() []GenreMajor {
	result := make([]GenreMajor, 0, len(aribContentType))
	for _, entry := range aribContentType {
		middle := make([]GenreMiddle, 0, len(entry.Middle))
		for _, item := range entry.Middle {
			middle = append(middle, GenreMiddle{Key: item.Key, Name: item.Name})
		}
		result = append(result, GenreMajor{Key: entry.Key, Major: entry.Major, Middle: middle})
	}
	return result
}

// UserTypes は ariblib.constants.USER_TYPE 相当を挿入順で返す。
func UserTypes() []GenreMiddle {
	result := make([]GenreMiddle, 0, len(aribUserType))
	for _, entry := range aribUserType {
		result = append(result, GenreMiddle{Key: entry.Key, Name: entry.Name})
	}
	return result
}

// FindGenreMajor は大分類 (content_nibble_level1) に一致するエントリを返す。
func FindGenreMajor(key int) (GenreMajor, bool) {
	for _, entry := range aribContentType {
		if entry.Key != key {
			continue
		}
		middle := make([]GenreMiddle, 0, len(entry.Middle))
		for _, item := range entry.Middle {
			middle = append(middle, GenreMiddle{Key: item.Key, Name: item.Name})
		}
		return GenreMajor{Key: entry.Key, Major: entry.Major, Middle: middle}, true
	}
	return GenreMajor{}, false
}

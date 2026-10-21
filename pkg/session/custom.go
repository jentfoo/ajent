package session

import "encoding/json"

// LatestCustom decodes the newest custom entry of customType on branch into v,
// reporting whether one was found and decoded. Entries are latest-wins, so a
// caller that appends its state at every change reads back only the last.
func LatestCustom(branch []Entry, customType string, v any) bool {
	for i := len(branch) - 1; i >= 0; i-- {
		e := branch[i]
		if e.Type != TypeCustom {
			continue
		}
		var cd CustomData
		if err := e.Decode(&cd); err != nil || cd.CustomType != customType {
			continue
		}
		if len(cd.Data) == 0 {
			return false
		}
		return json.Unmarshal(cd.Data, v) == nil
	}
	return false
}

// CustomLanded reports whether any message entry follows the newest custom
// entry of customType on branch, telling a caller that what the entry recorded
// actually began rather than sitting alone at the tail. With no entry of the
// type it reports true: nothing can be pending.
func CustomLanded(branch []Entry, customType string) bool {
	for i := len(branch) - 1; i >= 0; i-- {
		e := branch[i]
		switch e.Type {
		case TypeMessage:
			return true
		case TypeCustom:
			var cd CustomData
			if err := e.Decode(&cd); err == nil && cd.CustomType == customType {
				return false
			}
		}
	}
	return true
}

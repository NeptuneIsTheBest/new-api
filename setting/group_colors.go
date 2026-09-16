package setting

import (
	"fmt"
	"maps"
	"strings"
	"sync"

	"github.com/QuantumNous/new-api/common"
)

const GroupColorsOptionKey = "GroupColors"

var groupColors = map[string]string{}
var groupColorsMutex sync.RWMutex

func parseGroupColors(value string) (map[string]string, error) {
	var colors map[string]string
	if err := common.UnmarshalJsonStr(value, &colors); err != nil {
		return nil, fmt.Errorf("GroupColors must be a JSON object of group names and preset colors: %w", err)
	}
	if colors == nil {
		return nil, fmt.Errorf("GroupColors must be a JSON object")
	}
	for group, color := range colors {
		if group == "" || group != strings.TrimSpace(group) || group == "auto" {
			return nil, fmt.Errorf("invalid group name in GroupColors: %q", group)
		}
		switch color {
		case "blue", "green", "cyan", "purple", "pink", "red", "orange", "yellow", "grey":
		default:
			return nil, fmt.Errorf("invalid preset color for group %q: %q", group, color)
		}
	}
	return colors, nil
}

func ValidateGroupColors(value string) error {
	_, err := parseGroupColors(value)
	return err
}

func UpdateGroupColorsByJSONString(value string) error {
	colors, err := parseGroupColors(value)
	if err != nil {
		return err
	}
	groupColorsMutex.Lock()
	defer groupColorsMutex.Unlock()
	groupColors = colors
	return nil
}

func GetGroupColorsCopy() map[string]string {
	groupColorsMutex.RLock()
	defer groupColorsMutex.RUnlock()
	return maps.Clone(groupColors)
}

func GroupColors2JSONString() string {
	data, err := common.Marshal(GetGroupColorsCopy())
	if err != nil {
		common.SysError("failed to marshal group colors: " + err.Error())
		return "{}"
	}
	return string(data)
}

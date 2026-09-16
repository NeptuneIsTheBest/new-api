package controller

import (
	"net/http"

	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"

	"github.com/gin-gonic/gin"
)

func GetGroups(c *gin.Context) {
	groupNames := make([]string, 0)
	configuredColors := setting.GetGroupColorsCopy()
	groupColors := make(map[string]string)
	for groupName := range ratio_setting.GetGroupRatioCopy() {
		groupNames = append(groupNames, groupName)
		if color, ok := configuredColors[groupName]; ok {
			groupColors[groupName] = color
		}
	}
	c.JSON(http.StatusOK, gin.H{
		"success":      true,
		"message":      "",
		"data":         groupNames,
		"group_colors": groupColors,
	})
}

func GetUserGroups(c *gin.Context) {
	usableGroups := make(map[string]map[string]any)
	userGroup := ""
	userId := c.GetInt("id")
	userGroup, _ = model.GetUserGroup(userId, false)
	userUsableGroups := service.GetUserUsableGroups(userGroup)
	configuredColors := setting.GetGroupColorsCopy()
	groupColors := make(map[string]string)
	for groupName := range ratio_setting.GetGroupRatioCopy() {
		// UserUsableGroups contains the groups that the user can use
		if desc, ok := userUsableGroups[groupName]; ok {
			usableGroups[groupName] = map[string]any{
				"ratio": service.GetUserGroupRatio(userGroup, groupName),
				"desc":  desc,
			}
			if color, ok := configuredColors[groupName]; ok {
				groupColors[groupName] = color
			}
		}
	}
	if _, ok := userUsableGroups["auto"]; ok {
		usableGroups["auto"] = map[string]any{
			"ratio": "自动",
			"desc":  setting.GetUsableGroupDescription("auto"),
		}
	}
	c.JSON(http.StatusOK, gin.H{
		"success":      true,
		"message":      "",
		"data":         usableGroups,
		"group_colors": groupColors,
	})
}

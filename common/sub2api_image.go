package common

import (
	"math"
	"strconv"
	"strings"
)

// Sub2APIImageRatio is the subscription image endpoint's supported composition set.
func Sub2APIImageRatio(ratio string) bool {
	switch ratio {
	case "auto", "1:1", "3:2", "2:3", "4:3", "3:4", "16:9", "9:16":
		return true
	}
	return false
}

// Sub2APIImageSizeMatches checks composition, not a provider-native pixel table.
func Sub2APIImageSizeMatches(ratio string, width, height int) bool {
	if width <= 0 || height <= 0 || !Sub2APIImageRatio(ratio) {
		return false
	}
	if ratio == "auto" {
		return true
	}
	parts := strings.Split(ratio, ":")
	a, _ := strconv.Atoi(parts[0])
	b, _ := strconv.Atoi(parts[1])
	return math.Abs(float64(width)/float64(height)/(float64(a)/float64(b))-1) <= 0.03+1e-12
}

package model

import "fmt"

type ChannelConsumptionData struct {
	ChannelID   int    `json:"channel_id" gorm:"column:channel_id"`
	ChannelName string `json:"channel_name" gorm:"-"`
	Quota       int    `json:"quota" gorm:"column:quota"`
}

func GetChannelConsumptionData(startTime int64, endTime int64) ([]ChannelConsumptionData, error) {
	rows := make([]ChannelConsumptionData, 0)
	err := DB.Table("quota_data").
		Select("channel_id, sum(quota) as quota").
		Where("channel_id > 0").
		Where("created_at >= ? and created_at <= ?", startTime, endTime).
		Group("channel_id").
		Having("sum(quota) > 0").
		Order("quota DESC, channel_id ASC").
		Find(&rows).Error
	if err != nil || len(rows) == 0 {
		return rows, err
	}

	channelIDs := make([]int, 0, len(rows))
	for _, row := range rows {
		channelIDs = append(channelIDs, row.ChannelID)
	}
	channelNames, err := getChannelNamesByIDs(channelIDs)
	if err != nil {
		return nil, err
	}
	for i := range rows {
		if name := channelNames[rows[i].ChannelID]; name != "" {
			rows[i].ChannelName = name
			continue
		}
		rows[i].ChannelName = fmt.Sprintf("channel-%d", rows[i].ChannelID)
	}
	return rows, nil
}

package runtimetui

import (
	"fmt"
	"strings"

	"submux/internal/runtimeapi"
)

func formatTrafficBytes(value uint64) string {
	const unit = 1024
	if value < unit {
		return fmt.Sprintf("%d B", value)
	}
	divisor := float64(unit)
	suffix := "KiB"
	if value >= unit*unit {
		divisor = float64(unit * unit)
		suffix = "MiB"
	}
	if value >= unit*unit*unit {
		divisor = float64(unit * unit * unit)
		suffix = "GiB"
	}
	return fmt.Sprintf("%.1f %s", float64(value)/divisor, suffix)
}

func (m Model) renderTrafficSparkline(upload bool) string {
	samples := m.visibleTrafficSamples()
	if len(samples) == 0 {
		return mutedStyle.Render("尚无样本")
	}
	width := m.width - 16
	if width < 24 {
		width = 24
	}
	if width > 72 {
		width = 72
	}
	if len(samples) > width {
		compact := make([]runtimeapi.TrafficSample, 0, width)
		for index := range width {
			compact = append(compact, samples[index*len(samples)/width])
		}
		samples = compact
	}
	maximum := uint64(0)
	for _, sample := range samples {
		value := sample.DownloadSpeed
		if upload {
			value = sample.UploadSpeed
		}
		if value > maximum {
			maximum = value
		}
	}
	levels := []rune("▁▂▃▄▅▆▇█")
	var chart strings.Builder
	for _, sample := range samples {
		if sample.Discontinuity {
			chart.WriteRune('│')
			continue
		}
		value := sample.DownloadSpeed
		if upload {
			value = sample.UploadSpeed
		}
		level := 0
		if maximum > 0 {
			level = int(value * uint64(len(levels)-1) / maximum)
		}
		chart.WriteRune(levels[level])
	}
	return chart.String() + "  峰值 " + formatTrafficBytes(maximum) + "/s"
}

func (m Model) visibleTrafficSamples() []runtimeapi.TrafficSample {
	if len(m.trafficHistory) == 0 {
		return nil
	}
	cutoff := m.trafficHistory[len(m.trafficHistory)-1].ObservedAt.Add(-m.trafficRange)
	first := 0
	for first < len(m.trafficHistory) && m.trafficHistory[first].ObservedAt.Before(cutoff) {
		first++
	}
	return m.trafficHistory[first:]
}

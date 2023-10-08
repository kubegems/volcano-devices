/*
Copyright 2023 The Volcano Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package config

import (
	"context"
	"strconv"

	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog"
	"volcano.sh/k8s-device-plugin/pkg/lock"
)

var (
	DeviceSplitCount   uint
	DeviceCoresScaling float64
	NodeName           string
	RuntimeSocketFlag  string
	DisableCoreLimit   bool
	DeviceListStrategy string
)

const (
	// LabelDeviceSplitCount = "volcano.sh/device-split-count"
	LabelDeviceSplitCount = "pai.kubegems.io/vgpu-per-card"
)

func GetDeviceSplitCount() uint {
	node, err := lock.GetClient().CoreV1().Nodes().Get(context.Background(), NodeName, v1.GetOptions{})
	if err != nil {
		klog.Infof("get node %s error %v", NodeName, err)
		return DeviceSplitCount
	}
	val, ok := node.Labels[LabelDeviceSplitCount]
	if !ok {
		return DeviceSplitCount
	}
	count, err := strconv.Atoi(val)
	if err != nil {
		klog.Infof("parse node label device-split-count %s error %v", val, err)
		return DeviceSplitCount
	}
	return uint(count)
}

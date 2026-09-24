package agent

import (
	"fmt"

	"project-alpha/internal/httpapi"
)

const maxReportConcurrency = 16

type reportSource struct {
	Concurrency int    `json:"concurrency"`
	SnapshotID  string `json:"snapshot_id"`
	Revision    int64  `json:"revision"`
	Scope       string `json:"scope"`
}

// The report contract lives on the server; the browser selects its source and concurrency.
const diskReportRequest = `生成容器空间分析报告：查清空间用途，找出可清理内容及处理条件。按可立即删除、存在争议、必须保留、放错位置四类归并，列出具体路径、实际占用及判断依据。报告不执行删除。`
const hostReportRequest = `生成 Host 空间分析报告：分析扫描记录中未被容器引用的空间，找出可清理内容及处理条件。按可立即删除、存在争议、必须保留、放错位置四类归并，列出具体路径、实际占用及判断依据。报告不执行删除。`

func (a *Manager) validateReportSource(source *reportSource) error {
	if source.Scope != "container" && source.Scope != "host" {
		return httpapi.NewError(400, "scope 必须为 host 或 container")
	}
	if source.Concurrency < 1 || source.Concurrency > maxReportConcurrency {
		return httpapi.NewError(400, "concurrency 必须为 1–16 的整数")
	}
	job, err := a.records.Job(source.SnapshotID)
	if err != nil {
		return err
	}
	if job["status"] != "completed" || job["trigger"] == "incremental" {
		return httpapi.NewError(409, "请选择已完成的扫描记录生成报告")
	}
	if fmt.Sprint(job["snapshot_revision"]) != fmt.Sprint(source.Revision) {
		return httpapi.NewError(409, "扫描记录已更新，请刷新空间用量后重新生成报告")
	}
	return nil
}

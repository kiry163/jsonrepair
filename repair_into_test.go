package jsonrepair

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

type repairIntoProfile struct {
	ID string `json:"id"`
}

type repairIntoResponse struct {
	Answer  string            `json:"answer"`
	Success bool              `json:"success"`
	Age     int               `json:"age"`
	Timeout int               `json:"timeout"`
	Profile repairIntoProfile `json:"profile"`
}

func TestRepairIntoValidJSON(t *testing.T) {
	var got repairIntoResponse
	report, err := RepairInto(`{"answer":"ok","success":true}`, &got)
	if err != nil {
		t.Fatalf("RepairInto() error = %v", err)
	}
	if report.Strategy != StrategyDirect {
		t.Fatalf("strategy = %q, want %q", report.Strategy, StrategyDirect)
	}
	want := repairIntoResponse{Answer: "ok", Success: true}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("result = %#v, want %#v", got, want)
	}
}

func TestRepairIntoUsesGenericRepairForMalformedJSON(t *testing.T) {
	var got repairIntoResponse
	report, err := RepairInto(`{'answer':'ok', success:true,}`, &got)
	if err != nil {
		t.Fatalf("RepairInto() error = %v", err)
	}
	if report.Strategy != StrategyGeneric {
		t.Fatalf("strategy = %q, want %q", report.Strategy, StrategyGeneric)
	}
	if got.Answer != "ok" || !got.Success {
		t.Fatalf("result = %#v", got)
	}
}

func TestRepairIntoIsAtomicOnSchemaError(t *testing.T) {
	got := repairIntoResponse{Answer: "old", Success: true}
	_, err := RepairInto(`{"answer":123}`, &got)
	if err == nil {
		t.Fatal("RepairInto() error = nil, want type mismatch")
	}
	if got.Answer != "old" || !got.Success {
		t.Fatalf("target changed after failure: %#v", got)
	}
	var repairErr *RepairError
	if !errors.As(err, &repairErr) || repairErr.Code != ErrorSchemaMismatch {
		t.Fatalf("error = %T %v, want schema mismatch", err, err)
	}
}

func TestRepairIntoUnknownFieldPolicies(t *testing.T) {
	input := `{"answer":"ok","extra":true}`

	var warned repairIntoResponse
	report, err := RepairInto(input, &warned)
	if err != nil {
		t.Fatalf("default policy error = %v", err)
	}
	if !hasIssue(report, IssueUnknownField, SeverityWarning) {
		t.Fatalf("default report = %#v, want unknown-field warning", report.Issues)
	}

	var rejected repairIntoResponse
	_, err = RepairInto(input, &rejected, WithUnknownFields(UnknownReject))
	if err == nil {
		t.Fatal("UnknownReject error = nil")
	}

	var strict repairIntoResponse
	_, err = RepairInto(input, &strict, WithStrict())
	if err == nil {
		t.Fatal("WithStrict() error = nil for unknown field")
	}

	var overridden repairIntoResponse
	_, err = RepairInto(input, &overridden, WithStrict(), WithUnknownFields(UnknownWarn))
	if err != nil {
		t.Fatalf("explicit UnknownWarn error = %v", err)
	}
}

func TestRepairIntoRequiredFields(t *testing.T) {
	input := `{"profile":{}}`

	var bestEffort repairIntoResponse
	report, err := RepairInto(input, &bestEffort, WithRequiredFields("profile.id"))
	if err != nil {
		t.Fatalf("best effort error = %v", err)
	}
	if !hasIssue(report, IssueMissingField, SeverityWarning) {
		t.Fatalf("report = %#v, want missing-field warning", report.Issues)
	}

	var strict repairIntoResponse
	_, err = RepairInto(input, &strict, WithStrict(), WithRequiredFields("profile.id"))
	if err == nil {
		t.Fatal("strict required-field error = nil")
	}
}

func TestRepairIntoTypeCoercion(t *testing.T) {
	input := `{"age":"30"}`

	var without repairIntoResponse
	_, err := RepairInto(input, &without)
	if err == nil {
		t.Fatal("without coercion error = nil")
	}

	var with repairIntoResponse
	report, err := RepairInto(input, &with, WithTypeCoercion(true))
	if err != nil {
		t.Fatalf("with coercion error = %v", err)
	}
	if with.Age != 30 {
		t.Fatalf("age = %d, want 30", with.Age)
	}
	if !hasIssue(report, IssueTypeCoercion, SeverityWarning) {
		t.Fatalf("report = %#v, want coercion warning", report.Issues)
	}
}

func TestRepairIntoOpaqueJSONTypes(t *testing.T) {
	type opaqueResponse struct {
		At   time.Time       `json:"at"`
		Data json.RawMessage `json:"data"`
	}

	var got opaqueResponse
	_, err := RepairInto(`{"at":"2025-01-02T03:04:05Z","data":{"ok":true}}`, &got)
	if err != nil {
		t.Fatalf("RepairInto() error = %v", err)
	}
	if got.At.IsZero() || string(got.Data) != `{"ok":true}` {
		t.Fatalf("result = %#v", got)
	}
}

func TestRepairIntoRejectsInvalidTarget(t *testing.T) {
	if _, err := RepairInto(`{}`, repairIntoResponse{}); err == nil {
		t.Fatal("non-pointer target error = nil")
	}
	if _, err := RepairInto(`{}`, (*repairIntoResponse)(nil)); err == nil {
		t.Fatal("nil target error = nil")
	}
}

func hasIssue(report *RepairReport, code IssueCode, severity Severity) bool {
	for _, issue := range report.Issues {
		if issue.Code == code && issue.Severity == severity {
			return true
		}
	}
	return false
}

func TestRepairIntoReportJSONIsValid(t *testing.T) {
	var got repairIntoResponse
	report, err := RepairInto(`{'answer':'ok'}`, &got)
	if err != nil {
		t.Fatalf("RepairInto() error = %v", err)
	}
	if !json.Valid([]byte(report.RepairedJSON)) {
		t.Fatalf("report JSON is invalid: %q", report.RepairedJSON)
	}
}

func TestRepairIntoGuidedStringBoundary(t *testing.T) {
	input := `{"answer":"foo", "unknown": 30, bar","success":true}`
	var got repairIntoResponse
	report, err := RepairInto(input, &got)
	if err != nil {
		t.Fatalf("RepairInto() error = %v (report=%#v)", err, report)
	}
	if report.Strategy != StrategyGuided {
		t.Fatalf("strategy = %q, want %q", report.Strategy, StrategyGuided)
	}
	if !hasIssue(report, IssueAmbiguousBoundary, SeverityWarning) {
		t.Fatalf("issues = %#v, want ambiguity warning", report.Issues)
	}
	if got.Answer != `foo", "unknown": 30, bar` || !got.Success {
		t.Fatalf("result = %#v", got)
	}
}

func TestRepairIntoGuidedStringAndExternalField(t *testing.T) {
	input := `{"answer":"说明中包含 "timeout": 30","timeout":60,"success":true}`
	var got repairIntoResponse
	_, err := RepairInto(input, &got)
	if err != nil {
		t.Fatalf("RepairInto() error = %v", err)
	}
	if got.Answer != `说明中包含 "timeout": 30` || got.Timeout != 60 || !got.Success {
		t.Fatalf("result = %#v", got)
	}
}

func TestRepairIntoQuotedBracesAndBracketsInNestedResult(t *testing.T) {
	type dimensionResult struct {
		DimensionID int    `json:"dimension_id"`
		Result      string `json:"result"`
	}
	type reviewResult struct {
		DimensionResults  []dimensionResult `json:"dimension_results"`
		OverallConclusion string            `json:"overall_conclusion"`
	}

	input := `{"dimension_results": [{"dimension_id": 38, "result": "存在以下真实性问题：1）作者单位邮编"3501001"为7位，中国邮政编码应为6位，疑为"350100"多录入一位；2）参考文献[4]标注发表年份"2026年"，当前为2025年，文献不可能已发表，需核实真实年份或更换；3）《建筑电气工程施工质量验收规范》（GB 50303）未标注具体年份版本号（现行版本为GB 50303-2015），建议补充；4）表1各场景准确率均值计算无误，摘要"提升约23个百分点"与综合均值22.2个百分点基本吻合；5）湛江市住建局通报文件经核实确实存在，建议核对文中具体项目名称是否与原文完全一致。"}, {"dimension_id": 40, "result": "稿件在表达规范性方面存在以下主要问题：【语法】多处成分残缺与句式杂糅，长句中缺少必要逗号导致语义粘连；"为背景支撑"搭配不当；"较人工巡检提前发现风险平均时长缩短至2.4天"语序不当。【字词】公式中出现拼写错误"$\\arg{mak}$"应为"$\\arg{max}$"；公式说明中"第ii i条"为排版错误，应为"第$i$条"。【标点】并列成分之间逗号与顿号混用突出：摘要"质量要素，施工缺陷，规范条文"、1.1节"低压配电，防雷接地，电气照明，线缆敷设"、4节"质量规范语义化，缺陷推理自动化与整改管控闭环化"中逗号均应改为顿号；2.1节三个并列分句内部已有逗号，分句间应使用分号。【术语】多个非通用缩写首次出现未标注全称：Neo4j、SWRL、BiLSTM-CRF、BERT、SPARQL、HermiT、OWL DL、LLM等均需在首次出现时补充全称及说明。建议系统修订后重新提交。"}], "overall_conclusion": "稿件《建筑电气工程施工质量知识图谱构建与民居应用实践》选题契合智能建造与知识图谱行业应用前沿，应用价值较为突出，实验数据详实，整体结构合理、题文相符、要素适配良好。但存在以下主要问题需修改：1）题目中"民居"与正文研究对象（城镇住宅小区）不匹配，建议修正；2）引言缺失"研究问题及方法"概述，结构不完整；3）结语未逐一回应引言提出的研究空白，展望部分引入正文未涉及的新概念（LLM融合、知识联邦）；4）存在数据矛盾（整改周期缩短比例不一致）、公式推导步骤缺失、方法-结果传导链断裂等逻辑闭环问题；5）参考文献仅6条且存在年份错误（2026年）、主题适配性不足等问题；6）表达规范性方面存在标点混用、术语缩写未标注全称、字词错误等较多问题；7）第3.1节引用真实政府通报及具体项目名称，存在敏感内容风险，需脱敏处理；8）学术创新性不足，更接近应用实践报告。建议作者针对上述问题进行系统性修改后重新提交审查。"}`

	var got reviewResult
	report, err := RepairInto(input, &got)
	if err != nil {
		t.Fatalf("RepairInto() error = %v (report=%#v)", err, report)
	}
	if report.Strategy != StrategyGuided {
		t.Fatalf("strategy = %q, want %q", report.Strategy, StrategyGuided)
	}
	if len(got.DimensionResults) != 2 || got.DimensionResults[0].DimensionID != 38 || got.DimensionResults[1].DimensionID != 40 {
		t.Fatalf("dimension results = %#v", got.DimensionResults)
	}
	if !strings.Contains(got.DimensionResults[1].Result, `arg{max}`) {
		t.Fatalf("formula content was truncated: %q", got.DimensionResults[1].Result)
	}
	if !strings.Contains(got.DimensionResults[0].Result, "是否与原文完全一致") ||
		!strings.Contains(got.DimensionResults[1].Result, "建议系统修订后重新提交") {
		t.Fatalf("dimension result content was truncated: %#v", got.DimensionResults)
	}
	if !strings.Contains(got.DimensionResults[0].Result, "真实性问题：") {
		t.Fatalf("string punctuation was normalized: %q", got.DimensionResults[0].Result)
	}
	if !strings.Contains(got.OverallConclusion, "学术创新性不足") {
		t.Fatalf("overall conclusion was truncated: %q", got.OverallConclusion)
	}
}

package proxy

// ActiveCodexHarvestManagerForTest 暴露进程级采票管理器，供 wsrelay 等外部包测试
// 装配与还原，生产代码请继续使用包内 codexHarvester。
func ActiveCodexHarvestManagerForTest() *CodexHarvestManager {
	return codexHarvester.Load()
}

// SetActiveCodexHarvestManagerForTest 还原进程级采票管理器指针。
func SetActiveCodexHarvestManagerForTest(m *CodexHarvestManager) {
	codexHarvester.Store(m)
}

// CodexTicketFeedbackSinkForTest 是反馈回写目标的别名。
type CodexTicketFeedbackSinkForTest = codexTicketFeedbackSink

// ActiveCodexTicketFeedbackForTest 暴露当前反馈回写目标。
func ActiveCodexTicketFeedbackForTest() *CodexTicketFeedbackSinkForTest {
	return codexTicketFeedback.Load()
}

// RestoreCodexTicketFeedbackForTest 还原反馈回写目标指针。
func RestoreCodexTicketFeedbackForTest(sink *CodexTicketFeedbackSinkForTest) {
	codexTicketFeedback.Store(sink)
}

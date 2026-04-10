package builtins

// AuxotOpenUIChatSkillFooter is appended to the auxot:openui skill body when returned via useSkill
// so the model knows chat renders OpenUI statically (no llm/tool datasources).
const AuxotOpenUIChatSkillFooter = `

---

**Chat rendering:** In the main chat (not the Apps canvas), OpenUI is shown **statically**. llm(...) and tool(...) data sources **do not run**. Gather data with your normal tools first, then **hard-code** literals (arrays, numbers, strings) in the openui code fence so the UI renders without live data sources.
`

// AppendAuxotOpenUIChatFooterIfNeeded appends AuxotOpenUIChatSkillFooter when skillID is auxot:openui.
func AppendAuxotOpenUIChatFooterIfNeeded(skillID, content string) string {
	if skillID != "auxot:openui" {
		return content
	}
	return content + AuxotOpenUIChatSkillFooter
}

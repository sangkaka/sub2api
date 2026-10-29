package domain

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// 重构前各处手写的平台列表，作为平台清单派生结果的等价基准；重构后新登记的平台
// （Command Code、Cline、Leonardo、Fish、TRAE）按同类平台的位置补入。
var (
	legacyDisplayOrder = []string{
		PlatformAnthropic, PlatformOpenAI, PlatformGemini, PlatformAntigravity, PlatformKiro, PlatformGrok, PlatformAdobe,
		PlatformKimi, PlatformZhipu, PlatformDeepseek, PlatformMiniMax, PlatformOpenCodeGo, PlatformTypeSafe,
		PlatformCommandCode,
		PlatformCline,
		PlatformLeonardo,
		PlatformFish,
		PlatformTrae,
	}
	legacyCompositePrecedence = []string{
		PlatformAnthropic, PlatformGemini, PlatformOpenAI, PlatformAntigravity, PlatformKiro, PlatformGrok, PlatformAdobe,
		PlatformKimi, PlatformZhipu, PlatformDeepseek, PlatformMiniMax, PlatformOpenCodeGo, PlatformTypeSafe,
		PlatformCommandCode,
		PlatformCline,
		PlatformLeonardo,
		PlatformFish,
		PlatformTrae,
	}
	legacyOpenAIGateway = []string{
		PlatformOpenAI, PlatformGrok, PlatformKimi, PlatformZhipu, PlatformDeepseek, PlatformMiniMax, PlatformOpenCodeGo,
		PlatformCommandCode,
		PlatformCline,
		PlatformFish,
		PlatformTrae,
	}
	legacyCNProviders    = []string{PlatformKimi, PlatformZhipu, PlatformDeepseek, PlatformMiniMax}
	legacyLiteLLMByPlatf = map[string]string{
		PlatformAnthropic: "anthropic", PlatformOpenAI: "openai", PlatformGemini: "gemini",
		PlatformAntigravity: "anthropic", PlatformGrok: "xai", PlatformKimi: "moonshot",
		PlatformZhipu: "zhipu", PlatformDeepseek: "deepseek", PlatformMiniMax: "minimax",
		PlatformOpenCodeGo: "opencode-go", PlatformTypeSafe: "typesafe", PlatformCommandCode: "", PlatformCline: "",
		PlatformKiro: "", PlatformAdobe: "",
		PlatformLeonardo: "",
		PlatformFish: "",
		PlatformTrae:  "",
	}
)

func TestPlatformListMatchesLegacyLists(t *testing.T) {
	require.Equal(t, legacyDisplayOrder, ConcretePlatformIDs())
	require.Equal(t, legacyCompositePrecedence, CompositePrecedencePlatformIDs())
	require.Equal(t, legacyOpenAIGateway, PlatformIDsWhere(func(spec PlatformSpec) bool {
		return spec.Gateway == PlatformGatewayOpenAI
	}))
	require.Equal(t, legacyCNProviders, PlatformIDsWhere(func(spec PlatformSpec) bool { return spec.CNProvider }))
	for platform, provider := range legacyLiteLLMByPlatf {
		spec, ok := LookupPlatform(platform)
		require.True(t, ok, platform)
		require.Equal(t, provider, spec.LiteLLMProvider, platform)
	}
}

func TestPlatformListPredicates(t *testing.T) {
	for _, platform := range legacyDisplayOrder {
		require.True(t, IsConcretePlatform(platform), platform)
		require.True(t, IsGroupPlatform(platform), platform)
		spec, ok := LookupPlatform(platform)
		require.True(t, ok, platform)
		require.NotEmpty(t, spec.DisplayName, platform)
		require.NotEmpty(t, spec.Gateway, platform)
	}
	require.False(t, IsConcretePlatform(PlatformComposite))
	require.True(t, IsGroupPlatform(PlatformComposite))
	for _, invalid := range []string{"", "moonshot", "Kimi", "openai ", "glm", "bogus"} {
		require.False(t, IsConcretePlatform(invalid), invalid)
		require.False(t, IsGroupPlatform(invalid), invalid)
		require.False(t, UsesOpenAIGateway(invalid), invalid)
	}
}

func TestPlatformsReturnsCopy(t *testing.T) {
	platforms := Platforms()
	platforms[0].ID = "mutated"
	require.Equal(t, PlatformAnthropic, Platforms()[0].ID)
}

// TRAE 是 OpenAI 网关族的私有协议平台：不参与 CN 供应商余额能力，也不从
// LiteLLM 定价目录同步（模型目录按账号从上游拉取），登记位置在清单尾部。
func TestTraePlatformRegistration(t *testing.T) {
	require.True(t, IsConcretePlatform(PlatformTrae))
	require.True(t, IsGroupPlatform(PlatformTrae))
	require.True(t, UsesOpenAIGateway(PlatformTrae))

	spec, ok := LookupPlatform(PlatformTrae)
	require.True(t, ok)
	require.Equal(t, "TRAE", spec.DisplayName)
	require.False(t, spec.CNProvider)
	require.Empty(t, spec.LiteLLMProvider)

	ids := ConcretePlatformIDs()
	require.Equal(t, PlatformTrae, ids[len(ids)-1], "trae must be registered at the tail")
	precedence := CompositePrecedencePlatformIDs()
	require.Equal(t, PlatformTrae, precedence[len(precedence)-1], "trae must be appended at composite precedence tail")
	require.Equal(t, []string{PlatformAnthropic, PlatformGemini, PlatformOpenAI}, precedence[:3],
		"composite precedence fixed prefix must stay unchanged")
}

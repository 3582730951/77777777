#!/usr/bin/env bash
# ============================================================================
#  配置 Codex CLI 连接 LLM Pool 网关
#
#  用法: bash setup-codex.sh <网关地址> <API Key>
#  示例: bash setup-codex.sh http://1.2.3.4:8787 sk-pool-xxxxx
#
#  此脚本修改:
#    ~/.codex/config.toml  — 设置 base_url 指向网关
#    ~/.codex/auth.json    — 设置 API key（API key 模式）
# ============================================================================

set -euo pipefail

GATEWAY_URL="${1:-}"
API_KEY="${2:-}"

if [[ -z "$GATEWAY_URL" || -z "$API_KEY" ]]; then
    echo "用法: bash setup-codex.sh <网关地址> <API Key>"
    echo "示例: bash setup-codex.sh http://1.2.3.4:8787 sk-pool-xxxxx"
    exit 1
fi

# 去掉末尾斜杠
GATEWAY_URL="${GATEWAY_URL%/}"

CODEX_DIR="$HOME/.codex"
mkdir -p "$CODEX_DIR"

# 写 config.toml
cat > "$CODEX_DIR/config.toml" << EOF
# LLM Pool Gateway 配置（由 setup-codex.sh 生成）
model_provider = "llmpool"
model = "gpt-5.2"
model_reasoning_effort = "high"
disable_response_storage = true

[model_providers.llmpool]
name = "llmpool"
base_url = "${GATEWAY_URL}"
wire_api = "responses"
EOF

echo "✓ 已写入 $CODEX_DIR/config.toml"
echo "  base_url = ${GATEWAY_URL}"

# 写 auth.json（API key 模式）
cat > "$CODEX_DIR/auth.json" << EOF
{
  "OPENAI_API_KEY": "${API_KEY}"
}
EOF
chmod 600 "$CODEX_DIR/auth.json"

echo "✓ 已写入 $CODEX_DIR/auth.json"
echo ""
echo "现在可以运行 codex 了。它会连接到 ${GATEWAY_URL}"
echo ""
echo "如果需要恢复为 OpenAI 官方:"
echo "  rm ~/.codex/config.toml"
echo "  codex auth login"

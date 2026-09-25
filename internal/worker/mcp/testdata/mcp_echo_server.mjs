#!/usr/bin/env node
// 最小 MCP stdio server —— 仅供 internal/worker/mcp 的集成测试使用。
//
// 它实现了触发真实传输层所需的最小闭环：
//   initialize  → 握手
//   tools/list  → 暴露一个 echo 工具
//   tools/call  → 回显入参
//
// 之所以要真进程而不是 fake transport：被修掉的 bug（TransportFactory 未接线）
// 恰恰只在「真的去拉起进程并按 JSON-RPC 对话」时才会暴露。
import readline from 'node:readline';

const rl = readline.createInterface({ input: process.stdin, terminal: false });

function send(msg) {
  process.stdout.write(JSON.stringify(msg) + '\n');
}

rl.on('line', (line) => {
  const text = line.trim();
  if (!text) return;

  let req;
  try {
    req = JSON.parse(text);
  } catch {
    return; // 非 JSON 行忽略
  }
  if (req.id === undefined) return; // 通知无需响应

  switch (req.method) {
    case 'initialize':
      send({
        jsonrpc: '2.0',
        id: req.id,
        result: {
          protocolVersion: '2024-11-05',
          capabilities: { tools: {} },
          serverInfo: { name: 'echo-test', version: '1.0.0' },
        },
      });
      break;

    case 'tools/list':
      send({
        jsonrpc: '2.0',
        id: req.id,
        result: {
          tools: [
            {
              name: 'echo',
              description: '把输入原样回显',
              inputSchema: {
                type: 'object',
                properties: { text: { type: 'string', description: '要回显的文本' } },
                required: ['text'],
              },
            },
          ],
        },
      });
      break;

    case 'tools/call': {
      const args = (req.params && req.params.arguments) || {};
      send({
        jsonrpc: '2.0',
        id: req.id,
        result: {
          content: [{ type: 'text', text: `echo: ${args.text ?? ''}` }],
          isError: false,
        },
      });
      break;
    }

    default:
      send({
        jsonrpc: '2.0',
        id: req.id,
        error: { code: -32601, message: `未知方法 ${req.method}` },
      });
  }
});

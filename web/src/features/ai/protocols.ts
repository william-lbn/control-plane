export type AIProtocol = 'chat_completions' | 'responses' | 'embeddings' | 'messages' | 'gemini';
export type AIPrice = {
  version: string;
  currency: 'USD';
  max_input_tokens: number;
  input_microusd_per_million_tokens: number;
  output_microusd_per_million_tokens: number;
  cache_read_microusd_per_million_tokens: number;
  cache_write_microusd_per_million_tokens: number;
};

export function usdToMicros(value: string): number {
  if (!/^(0|[1-9][0-9]{0,6})(\.[0-9]{1,6})?$/.test(value))
    throw new Error('预算使用最多六位小数的美元金额');
  const [whole, fraction = ''] = value.split('.');
  const amount = BigInt(whole) * 1000000n + BigInt(fraction.padEnd(6, '0'));
  if (amount > 1000000000000n) throw new Error('预算不能超过 1000000 USD');
  return Number(amount);
}

export const protocolNames: Record<AIProtocol, string> = {
  chat_completions: 'Chat Completions',
  responses: 'Responses',
  embeddings: 'Embeddings',
  messages: 'Messages',
  gemini: 'Gemini',
};

export function playgroundRequest(
  protocol: AIProtocol,
  model: string,
  prompt: string,
  maximum: number,
  stream: boolean,
) {
  const limit = Math.min(128, maximum);
  switch (protocol) {
    case 'responses':
      return { model, input: prompt, max_output_tokens: limit, stream, store: false };
    case 'embeddings':
      return { model, input: prompt, encoding_format: 'float' };
    case 'messages':
      return { model, messages: [{ role: 'user', content: prompt }], max_tokens: limit, stream };
    case 'gemini':
      return {
        model,
        contents: [{ role: 'user', parts: [{ text: prompt }] }],
        generationConfig: { maxOutputTokens: limit },
      };
    default:
      return {
        model,
        messages: [{ role: 'user', content: prompt }],
        max_tokens: limit,
        temperature: 0,
        stream,
      };
  }
}

// Display native tool objects without executing them in the Console.
export function protocolAnswer(protocol: AIProtocol, value: Record<string, any>): string {
  switch (protocol) {
    case 'responses':
      return (value.output || [])
        .map((item: any) =>
          item.type === 'message'
            ? (item.content || []).map((c: any) => c.text || JSON.stringify(c)).join('')
            : JSON.stringify(item),
        )
        .join('\n');
    case 'messages':
      return (value.content || [])
        .map((item: any) => (item.type === 'text' ? item.text : JSON.stringify(item)))
        .join('\n');
    case 'gemini':
      return (value.candidates || [])
        .flatMap((c: any) => c.content?.parts || [])
        .map((p: any) => p.text || JSON.stringify(p))
        .join('\n');
    case 'embeddings':
      return JSON.stringify(value.data, null, 2);
    default:
      return (
        value.choices?.[0]?.message?.content ||
        (value.choices?.[0]?.message?.tool_calls
          ? JSON.stringify(value.choices[0].message.tool_calls, null, 2)
          : '')
      );
  }
}

export function protocolDelta(
  protocol: AIProtocol,
  value: Record<string, any>,
): { text: string; done: boolean } {
  if (value.error || value.type === 'response.failed' || value.type === 'response.incomplete')
    throw new Error(value.error?.code || '推理流未正常结束；不会自动重试');
  if (protocol === 'responses')
    return {
      text:
        value.type === 'response.output_text.delta'
          ? value.delta || ''
          : value.type === 'response.function_call_arguments.delta'
            ? value.delta || ''
            : '',
      done: value.type === 'response.completed',
    };
  if (protocol === 'messages')
    return {
      text: value.delta?.text || value.delta?.partial_json || value.delta?.thinking || '',
      done: value.type === 'message_stop',
    };
  return {
    text:
      value.choices?.[0]?.delta?.content ||
      (value.choices?.[0]?.delta?.tool_calls
        ? JSON.stringify(value.choices[0].delta.tool_calls)
        : ''),
    done: false,
  };
}

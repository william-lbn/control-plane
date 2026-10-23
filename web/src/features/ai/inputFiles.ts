import type { AIInputFile } from './AIInputFiles';
import type { AIProtocol } from './protocols';

export function inputKind(mime: string): string {
  return mime.startsWith('image/')
    ? 'image'
    : mime === 'application/pdf'
      ? 'pdf'
      : mime.startsWith('audio/')
        ? 'audio'
        : mime.startsWith('video/')
          ? 'video'
          : '';
}
export function attachInputFiles(
  protocol: AIProtocol,
  value: Record<string, any>,
  files: AIInputFile[],
  modalities: string[],
) {
  if (!files.length) return value;
  if (files.length > 8 || files.reduce((sum, v) => sum + v.bytes, 0) > 1048576)
    throw Error('本次文件总量超过限制');
  const body = structuredClone(value);
  const parts = files.map((file) => {
    const kind = inputKind(file.content_type);
    if (!kind || !modalities.includes(kind))
      throw Error('当前模型未批准此文件类型：' + file.filename);
    switch (protocol) {
      case 'responses':
        if (kind === 'image') return { type: 'input_image', file_id: file.id };
        if (kind === 'pdf') return { type: 'input_file', file_id: file.id };
        break;
      case 'messages':
        if (kind === 'image' || kind === 'pdf')
          return {
            type: kind === 'pdf' ? 'document' : 'image',
            source: { type: 'file', file_id: file.id },
          };
        break;
      case 'gemini':
        return { fileData: { fileUri: 'neon-file:' + file.id, mimeType: file.content_type } };
      case 'chat_completions':
        if (kind === 'image')
          return { type: 'image_url', image_url: { url: 'neon-file:' + file.id } };
        if (kind === 'pdf') return { type: 'file', file: { file_id: file.id } };
        if (kind === 'audio') return { type: 'input_audio', input_audio: { file_id: file.id } };
        break;
    }
    throw Error('当前协议不支持此文件引用：' + file.filename);
  });
  switch (protocol) {
    case 'responses': {
      const text = typeof body.input === 'string' ? [{ type: 'input_text', text: body.input }] : [];
      if (typeof body.input !== 'string' && !Array.isArray(body.input))
        throw Error('Responses input 格式无效');
      body.input =
        typeof body.input === 'string'
          ? [{ role: 'user', content: [...text, ...parts] }]
          : [...body.input, { role: 'user', content: parts }];
      break;
    }
    case 'gemini':
      if (!Array.isArray(body.contents)) throw Error('Gemini contents 格式无效');
      body.contents = [...body.contents, { role: 'user', parts }];
      break;
    case 'messages':
    case 'chat_completions':
      if (!Array.isArray(body.messages)) throw Error('messages 格式无效');
      body.messages = [...body.messages, { role: 'user', content: parts }];
      break;
    default:
      throw Error('Embeddings 不接受文件引用');
  }
  return body;
}

export function supportedInputTypes(protocol: AIProtocol, modalities: string[]): string[] {
  const supported =
    protocol === 'gemini'
      ? ['image', 'pdf', 'audio', 'video']
      : protocol === 'chat_completions'
        ? ['image', 'pdf', 'audio']
        : protocol === 'embeddings'
          ? []
          : ['image', 'pdf'];
  return modalities.filter((value) => supported.includes(value));
}

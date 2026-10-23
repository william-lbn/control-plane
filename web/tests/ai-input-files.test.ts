import assert from 'node:assert/strict';
import test from 'node:test';
import { attachInputFiles, supportedInputTypes } from '../src/features/ai/inputFiles.ts';
import type { AIInputFile } from '../src/features/ai/AIInputFiles.tsx';
import { playgroundRequest } from '../src/features/ai/protocols.ts';

const file: AIInputFile = {
  id: 'nif_111111111111111111111111',
  filename: 'probe.pdf',
  content_type: 'application/pdf',
  bytes: 100,
  sha256: 'a'.repeat(64),
  expires_at: 2000000000,
};
test('file selection preserves native input messages and gateway references', () => {
  const original = playgroundRequest('responses', 'alias', 'question', 64, false);
  const response = attachInputFiles('responses', original, [file], ['text', 'pdf']);
  assert.equal(original.input, 'question');
  assert.deepEqual(response.input, [
    {
      role: 'user',
      content: [
        { type: 'input_text', text: 'question' },
        { type: 'input_file', file_id: file.id },
      ],
    },
  ]);
  const message = attachInputFiles(
    'messages',
    playgroundRequest('messages', 'alias', 'question', 64, false),
    [file],
    ['text', 'pdf'],
  );
  assert.deepEqual(message.messages[1], {
    role: 'user',
    content: [{ type: 'document', source: { type: 'file', file_id: file.id } }],
  });
  const gemini = attachInputFiles(
    'gemini',
    playgroundRequest('gemini', 'alias', 'question', 64, false),
    [file],
    ['text', 'pdf'],
  );
  assert.deepEqual(gemini.contents[1], {
    role: 'user',
    parts: [{ fileData: { fileUri: 'neon-file:' + file.id, mimeType: 'application/pdf' } }],
  });
  assert(!JSON.stringify([response, message, gemini]).includes('base64'));
});
test('unapproved and excessive files are refused before inference construction', () => {
  const input = playgroundRequest('responses', 'alias', 'question', 64, false);
  assert.throws(() => attachInputFiles('responses', input, [file], ['text']), /未批准/);
  assert.throws(() => attachInputFiles('embeddings', input, [file], ['text', 'pdf']), /不支持/);
  assert.throws(
    () => attachInputFiles('responses', input, Array(9).fill(file), ['text', 'pdf']),
    /限制/,
  );
  assert.throws(
    () => attachInputFiles('responses', input, [{ ...file, bytes: 1048577 }], ['text', 'pdf']),
    /限制/,
  );
  assert.equal(attachInputFiles('responses', input, [], ['text']), input);
});
test('Chat audio and image references do not invent native Gemini or Messages payloads', () => {
  const audio = { ...file, content_type: 'audio/wav' },
    image = { ...file, id: 'nif_222222222222222222222222', content_type: 'image/png' };
  const chat = attachInputFiles(
    'chat_completions',
    playgroundRequest('chat_completions', 'alias', 'question', 64, false),
    [audio, image],
    ['text', 'audio', 'image'],
  );
  assert.deepEqual(chat.messages[1], {
    role: 'user',
    content: [
      { type: 'input_audio', input_audio: { file_id: audio.id } },
      { type: 'image_url', image_url: { url: 'neon-file:' + image.id } },
    ],
  });
  assert.deepEqual(supportedInputTypes('responses', ['text', 'image', 'pdf', 'audio', 'video']), [
    'image',
    'pdf',
  ]);
  assert.deepEqual(supportedInputTypes('gemini', ['text', 'audio', 'video']), ['audio', 'video']);
  assert.deepEqual(supportedInputTypes('embeddings', ['text', 'pdf']), []);
});

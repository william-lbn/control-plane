import { retryAfterSeconds } from './followOperation.ts';

export class ApiError extends Error {
  status: number;
  code: string;
  requestId?: string;
  retryAfterSeconds?: number;

  constructor(
    status: number,
    code: string,
    message: string,
    requestId?: string,
    retryAfter?: number,
  ) {
    super(message);
    this.status = status;
    this.code = code;
    this.requestId = requestId;
    this.retryAfterSeconds = retryAfter;
  }
}

// Never return a success-shaped empty object for a truncated/HTML response.
// Only the accepted-Operation observer retries reads; this parser does not
// repeat a request, and callers must retain mutation idempotency on uncertainty.
export async function decodeAPIResponse<T>(response: Response): Promise<T> {
  let value: unknown;
  try {
    value = await response.json();
  } catch {
    if (response.ok)
      throw new ApiError(502, 'invalid_api_response', '服务响应不完整，请继续查询已受理的操作。');
    throw new ApiError(
      response.status,
      'request_failed',
      `HTTP ${response.status}`,
      undefined,
      retryAfterSeconds(response.headers.get('Retry-After')),
    );
  }
  const data =
    value && typeof value === 'object' && !Array.isArray(value)
      ? (value as Record<string, unknown>)
      : null;
  if (!response.ok)
    throw new ApiError(
      response.status,
      typeof data?.code === 'string' ? data.code : 'request_failed',
      typeof data?.message === 'string' ? data.message : `HTTP ${response.status}`,
      typeof data?.request_id === 'string' ? data.request_id : undefined,
      retryAfterSeconds(response.headers.get('Retry-After')),
    );
  if (!data)
    throw new ApiError(502, 'invalid_api_response', '服务响应格式不正确，请继续查询已受理的操作。');
  return data as T;
}

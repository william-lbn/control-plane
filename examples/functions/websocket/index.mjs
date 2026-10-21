import { upgradeWebSocket } from '/opt/neon/functions/runtime/websocket.mjs';

export default request => {
  const url = new URL(request.url);
  if (url.pathname.endsWith('/deny')) return new Response('Application authorization required', { status: 401 });
  if (request.headers.get('upgrade')?.toLowerCase() !== 'websocket') return new Response('WebSocket endpoint', { status: 426 });
  const offers = request.headers.get('sec-websocket-protocol')?.split(',').map(p => p.trim()) || [];
  const { socket, response } = upgradeWebSocket(request, offers.includes('echo.v1') ? { protocol: 'echo.v1' } : undefined);
  socket.onopen = () => socket.send('ready');
  socket.onmessage = event => {
    if (event.data === 'server-close') socket.close(4001, 'done');
    else socket.send(event.data);
  };
  return response;
};

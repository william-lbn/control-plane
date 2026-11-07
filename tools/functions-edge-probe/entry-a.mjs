import { upgradeWebSocket } from '/opt/neon/functions/runtime/websocket.mjs';
import { createProbe } from './handler.mjs';
export default createProbe('A', upgradeWebSocket);

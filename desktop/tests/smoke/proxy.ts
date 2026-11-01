// A TCP proxy in front of a disposable conchd that can hold what the server
// sends back: the delayed response and the delayed WebSocket frame of issue
// #945's fencing test. It forwards bytes and understands nothing.
import { createConnection, createServer, type Server, type Socket } from 'node:net';

export interface DelayProxy {
  readonly origin: string;
  // Hold everything the server sends from now on.
  hold(): void;
  // How many bytes the server has sent that are being held now.
  heldBytes(): number;
  // What is being held, as text (the server's frames are not masked).
  heldText(): string;
  // Send what was held, in order, and stop holding.
  release(): void;
  stop(): Promise<void>;
}

export function delayProxy(targetPort: number): Promise<DelayProxy> {
  return new Promise((resolve, reject) => {
    let holding = false;
    const pairs: [Socket, Socket][] = [];
    const queued = new Map<Socket, Buffer[]>();
    const server: Server = createServer((client) => {
      const upstream = createConnection({ host: '127.0.0.1', port: targetPort });
      pairs.push([client, upstream]);
      queued.set(client, []);
      client.on('data', (chunk) => upstream.write(chunk));
      upstream.on('data', (chunk: Buffer) => {
        if (holding) queued.get(client)?.push(chunk);
        else client.write(chunk);
      });
      const end = (): void => {
        client.destroy();
        upstream.destroy();
      };
      client.on('error', end);
      upstream.on('error', end);
      client.on('close', end);
      upstream.on('close', () => {
        if (!holding) end();
      });
    });
    server.on('error', reject);
    server.listen(0, '127.0.0.1', () => {
      const address = server.address();
      const port = typeof address === 'object' && address !== null ? address.port : 0;
      resolve({
        origin: `http://127.0.0.1:${String(port)}`,
        hold: () => {
          holding = true;
        },
        heldText: () => [...queued.values()].flat().map((c) => c.toString('utf8')).join(''),
        heldBytes: () => [...queued.values()].reduce((n, chunks) => n + chunks.reduce((m, c) => m + c.length, 0), 0),
        release: () => {
          holding = false;
          for (const [client] of pairs) {
            for (const chunk of queued.get(client) ?? []) client.write(chunk);
            queued.set(client, []);
          }
        },
        stop: () =>
          new Promise<void>((done) => {
            for (const [client, upstream] of pairs) {
              client.destroy();
              upstream.destroy();
            }
            server.close(() => {
              done();
            });
          }),
      });
    });
  });
}

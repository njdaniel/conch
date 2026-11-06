// What the desktop adapter knows about the server it talks to: the canonical
// origin main accepted, or none until the person has entered one. It is the
// address, never a credential; services.origin() reads it.
let origin = '';

export function serverOrigin(): string {
  return origin;
}

export function setServerOrigin(next: string): void {
  origin = next;
}

// The shell page (ADR-007): it says what it is and which server it came from.
// Nothing calls the API yet; sign-in and the lobby come with #280.
export function App({ origin }: { readonly origin: string }) {
  return (
    <main>
      <h1>Conch</h1>
      <p>
        Server: <code>{origin}</code>
      </p>
    </main>
  );
}

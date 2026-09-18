// Wallet sign-in (did:pkh over EIP-4361). viem is loaded on demand so that the
// wallet code never lands in the initial bundle.

export interface WalletIdentity {
  did: string;
  address: string;
  chainId: number;
  signMessage(message: string): Promise<Uint8Array>;
}

interface Eip1193Provider {
  request(args: { method: string; params?: unknown[] }): Promise<unknown>;
}

function injected(): Eip1193Provider | null {
  const w = globalThis as unknown as { ethereum?: Eip1193Provider };
  return w.ethereum ?? null;
}

export function hasInjectedWallet(): boolean {
  return injected() !== null;
}

export function hexToBytes(hex: string): Uint8Array {
  const clean = hex.startsWith("0x") ? hex.slice(2) : hex;
  if (clean.length % 2 !== 0) throw new Error("hex: odd length");
  const out = new Uint8Array(clean.length / 2);
  for (let i = 0; i < out.length; i++) out[i] = parseInt(clean.slice(i * 2, i * 2 + 2), 16);
  return out;
}

export async function connectWallet(): Promise<WalletIdentity> {
  const provider = injected();
  if (!provider) throw new Error("No browser wallet found");
  const { createWalletClient, custom, getAddress } = await import("viem");
  const client = createWalletClient({ transport: custom(provider) });
  const [first] = await client.requestAddresses();
  if (!first) throw new Error("The wallet did not expose an account");
  const address = getAddress(first);
  const chainId = await client.getChainId();
  return {
    did: `did:pkh:eip155:${chainId}:${address}`,
    address,
    chainId,
    async signMessage(message) {
      const sig = await client.signMessage({ account: address, message });
      return hexToBytes(sig);
    },
  };
}

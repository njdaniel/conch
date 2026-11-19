export default function afterPack(context: {
  appOutDir: string;
  electronPlatformName: string;
}): Promise<void>;

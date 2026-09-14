import type { Metadata, Viewport } from "next";
import "./globals.css";
import { Providers } from "./providers";
import { AppShell } from "@/components/layout/app-shell";
import { AuthGate } from "@/components/layout/auth-gate";

export const metadata: Metadata = {
  title: "Agent Flow",
  description: "Run and supervise Claude Code and OpenCode terminals from your browser or phone.",
  manifest: "/manifest.json",
  appleWebApp: {
    capable: true,
    title: "Agent Flow",
    statusBarStyle: "black-translucent",
  },
};

// interactiveWidget resizes-content: the on-screen keyboard shrinks the layout
// viewport (Android/Chrome) so dvh-based chat shells adapt with zero JS. iOS
// Safari needs the visualViewport sync in the run shell — handled there.
export const viewport: Viewport = {
  width: "device-width",
  initialScale: 1,
  interactiveWidget: "resizes-content",
};

export default function RootLayout({
  children,
}: Readonly<{ children: React.ReactNode }>) {
  return (
    <html lang="en" suppressHydrationWarning>
      <body className="bg-background text-foreground min-h-screen font-sans antialiased">
        <Providers>
          <AuthGate>
            <AppShell>{children}</AppShell>
          </AuthGate>
        </Providers>
      </body>
    </html>
  );
}
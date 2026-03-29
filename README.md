# 🚀 StreamSrv — Real-Time WebRTC Streaming Platform

🎥 **Demo**  
https://youtu.be/y7a8Yw6csIc (with audio)
https://youtu.be/po8Xv_Q1oNA (no audio)

---

## ⚡ Overview

StreamSrv is a low-latency real-time streaming platform built with WebRTC that enables users to stream screen, camera, and audio directly to multiple viewers using peer-to-peer connections.

Unlike traditional streaming systems, **media is not processed or relayed through the server**. The backend handles only signaling and coordination, while video/audio flows directly between clients, resulting in minimal latency and efficient resource usage.

---

## 🔥 Key Highlights

- ⚡ Peer-to-peer streaming (no server media relay)
- 👥 Multi-viewer architecture (1 → N)
- 🎥 Screen sharing + camera overlay (PiP)
- 🎤 Independent mic / cam / screen control
- 💬 Real-time chat via WebSocket
- 🧠 Adaptive quality (ABR using WebRTC stats)
- 📊 Latency monitoring (RTT indicator)
- 🔐 JWT authentication system
- 🗄 PostgreSQL persistence (users, streams, chat)
- 🔄 Auto-reconnect with backoff

---

## 🧠 Architecture

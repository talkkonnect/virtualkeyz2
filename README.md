# VirtualKeyz2 — Operator Guide

Virtualkeyz2 is an opensource door and elevator PIN based access control using USB keypads and I2C expander boards for the hardware layer. Built for Fun Over the Songrkran Holidays!

This Go-based source code defines the core logic for VirtualKeyz, a versatile physical access control system designed for Raspberry Pi and similar hardware. The software manages security for doors and elevators by processing PIN-entry credentials and coordinating peripheral components like GPIO relays, I2C expanders, and audio feedback. It features a robust SQLite backend to store audit logs, user permissions, and complex scheduling rules that can enforce time-based restrictions or visitor lifecycles. Beyond local control, the system integrates with MQTT brokers and webhooks to support remote monitoring, event reporting, and synchronized multi-device configurations. A dedicated technician menu is included, allowing operators to adjust hardware settings, test relay outputs, and modify system behavior in real time through a terminal interface. Mixed-mode functionality also enables specialized elevator dispatching, where specific floors are energized based on authorized user profiles.

Read the OPERATIONS.md Document for More Information.

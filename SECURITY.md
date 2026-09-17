# 安全问题报告

**不要开公开 issue 报安全问题。** 公开 issue 在修复发布之前就把复现步骤给了所有人。

请走 GitHub 的私密漏洞报告：本仓库 **Security → Report a vulnerability**
（[私密安全公告](https://docs.github.com/code-security/security-advisories/guidance-on-reporting-and-writing-information-about-vulnerabilities/privately-reporting-a-security-vulnerability)）。
它是端到端私密的，只有仓库维护者看得到，也不需要你知道任何人的邮箱。

报告里请尽量给出：受影响的版本、复现步骤或最小代码、你认为的影响面。

## 支持的版本

本包处于 v0.x，**只修最新的小版本**。到 1.0 之后再谈支持窗口。

## 这个包的威胁模型

本包只做四件事：签名、验签、超时与有限重试、结构体。请按这个边界判断什么算漏洞：

**算**：签名/验签可被绕过或伪造；未验签的数据被当成已验签的数据使用；
时间戳窗口失效导致重放；`app_secret` 出现在错误信息里；
响应体读取没有上限导致内存被撑爆。

**不算**：接入方自己没验签、自己没做幂等；把 `app_secret` 写进源码或提交进 git；
`SkewWindow` 被自己调到很大之后的重放。这些是集成方式问题，
README 的「商户侧必须自己做的两件事」写明了边界。

服务端（jjpay 网关本身）的问题不在本仓库范围内，同样走上面的私密报告，我们会转到内部。
